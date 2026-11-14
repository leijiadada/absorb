#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""多模态协议冒烟（方案 B/A 落地验证，design/image-pipeline-plan.md 修订版）。

与现有三脚本（真实模型行为冒烟）定位分化：本脚本 = 协议冒烟——模型配置经
DBPatch 直写 SQLite 指向脚本内假端点，不依赖真实模型 key/网络，断言完全可控。

覆盖（1 探活 + 3 用例）：
  LIVE  /api/capabilities 探活：200 且含 imageSupport 字段；DBPatch 切换默认模型
        image_support 后取值翻转（堵 main.go bridge.Deps 装配盲区——单测不覆盖路由装配）
  MM1   1210 只提示不重试：元数据标 true 的纯文本模型（假端点返 400+1210）发图
        → SSE error code=model.image_unsupported，且带图请求恰好 1 次（无剥图重试；
        按带图请求数而非总数计数，避开 memory 提取器对同端点的异步调用污染）
  MM2   缩略图生命周期：发 800x600 PNG 大图（脚本手写 PNG，无第三方依赖）→
        当轮请求体为原图 → 会库落库缩略（jpeg <10KB，O6 直读）→ 同 scope 追问
        请求体重放缩略 part → 换 scope 追问 L1 注入缩略 part + 文本标记
  MM3   非视觉门控防回归：默认模型切 image_support=0 → 重启 → 同 scope 追问
        → 请求体不含任何 image part（openai 层 WithTextOnlyMessageContent 剥离）

数据源：
  O1 假端点收到的请求体（脚本内 http.server 捕获，按到达序记录）
  O2 L1 日志：data/logs/absorb.log 的 "working-set L1 | ..." 行
  O3 SSE：/api/ask 响应流
  O5 /api/capabilities 响应
  O6 会库：data/sessions.db 的 session_events.event JSON（sqlite3 直读，WAL 并发安全）

用法：python smoke_multimodal.py
前置：make build-cli（build/bin/absorb-agent-cli.exe）；7788 端口空闲；
      config.yaml 存在（脚本临时置空代理走直连假端点，退出恢复）。
产物：本目录 smoke_report_mm_<时间戳>.txt（跑完保留，供审查）。
注意：测试会话写入真实会话库（sessionId 前缀 mm-smoke-），按约定测试后重装清理。
"""

import base64
import json
import os
import re
import sqlite3
import struct
import subprocess
import sys
import threading
import time
import traceback
import urllib.request
import urllib.error
import zlib
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
AGENT_DIR = os.path.dirname(HERE)  # 仓库根（smoke/ 位于仓库内，config/build/data 均相对根）
CONFIG_PATH = os.path.join(AGENT_DIR, "config", "config.yaml")
CLI_EXE = os.path.join(AGENT_DIR, "build", "bin", "absorb-agent-cli.exe")
LOG_PATH = os.path.join(AGENT_DIR, "data", "logs", "absorb.log")
DB_PATH = os.path.join(AGENT_DIR, "data", "sessions.db")
BASE = "http://127.0.0.1:7788"
FAKE_PORT = 18777
FAKE_URL = "http://127.0.0.1:%d/chat/completions" % FAKE_PORT

SSE_TIMEOUT = 60
HEALTH_TIMEOUT = 40

# scope URL（父子分离三个独立 scope：P 同 scope 重放验证、Q 跨 scope L1 验证）
SMOKE_P = "https://mm-smoke.example.com/article-p"
SMOKE_Q = "https://mm-smoke.example.com/article-q"

APP_NAME = "absorb-reader"
USER_ID = "absorb-user"
THUMB_MAX_BYTES = 10 * 1024  # 方案 A 验收阈值：落库缩略 <10KB

# 本机代理未运行时模型调用报 "cannot connect to service"；假端点是 localhost，
# 置空 absorb 自身 proxy 配置保证直连（退出时随整文件备份恢复）
PROXY_LINE = '  url: "http://127.0.0.1:7897"         # HTTP 代理；国内直连 GitHub 等站点会被阻断，经本机代理出网'
PROXY_EMPTY = '  url: ""                             # HTTP 代理；冒烟临时置空走直连（退出时恢复）'

RE_L1 = re.compile(
    r"working-set L1 \| selected=(\d+) kept=(\d+) dropped_dup=(\d+) msgs=(\d+) est_tokens=(\d+)")

# ---------- 假图片（纯标准库手写 PNG，行内伪随机字节防 zlib 压缩，保证"原图"足够大） ----------

def make_png(w, h):
    """生成 w×h RGB PNG 的 dataURL。像素按 8px 量化形成低熵内容（模拟真实 UI 截图
    的大片平坦区），原图数十 KB、JPEG 缩略 3-5KB——高熵噪声图会让 q60 缩略虚胖到
    14KB+，不代表真实形态。"""
    def chunk(tag, data):
        return (struct.pack(">I", len(data)) + tag + data
                + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))

    def px(x, y):
        # 低频内容：坐标按 64px 网格量化（窗口/段落级平坦区，贴近真实 UI 截图；
        # 8px 量化在 192px 缩略后成 1.9px 高频纹理，JPEG 缩略会虚胖到 11KB+）
        qx, qy = x // 64, y // 64
        return bytes(((qx * 29 + qy * 17) & 0xFF, (qx * 7 + qy * 31) & 0xFF, (qx + qy * 11) & 0xFF))

    raw = b"".join(
        b"\x00" + b"".join(px(x, y) for x in range(w))  # 每行 1 字节 filter=0
        for y in range(h))
    ihdr = struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)  # 8bit RGB
    png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr)
           + chunk(b"IDAT", zlib.compress(raw, 1)) + chunk(b"IEND", b""))
    return "data:image/png;base64," + base64.b64encode(png).decode("ascii")


# ---------- 假 OpenAI 端点（协议级可控） ----------

class FakeEndpoint:
    """脚本内 http.server：1210/ok 双模式 + 请求体捕获。

    - mode="1210"：任何 POST 返回 400 + 扁平错误体（复刻真实 GLM 端点形态）；
    - mode="ok"：返回 openai chunk SSE（形态抄自 handler_multimodal_test.go defaultOKStream，
      经框架解析验证）；
    - full_url=1 语义下 CLI 直接 POST base_url 本身，路径不敏感（do_POST 兜底全部路径）。
    """

    def __init__(self, port):
        self.mode = "ok"
        self.requests = []  # [(path, body_bytes)] 按到达序
        self.lock = threading.Lock()
        outer = self

        class Handler(BaseHTTPRequestHandler):
            # HTTP/1.1 + 精确 Content-Length：HTTP/1.0 下 Go 客户端（openai-go）读不到
            # 400 错误体，raw_msg 只剩 "POST ...: 400 Bad Request "（body 空），1210 归一失败
            protocol_version = "HTTP/1.1"

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                body = self.rfile.read(length) if length else b""
                with outer.lock:
                    outer.requests.append((self.path, body))
                if outer.mode == "1210":
                    self.send_response(400)
                    self.send_header("Content-Type", "application/json")
                    # 错误体必须是 {"error":{...}} 包裹形态（真实 GLM 端点形态）：
                    # openai-go 的 requestconfig 只把 body 的 "error" 字段透传给
                    # apierror.Error（gjson.GetBytes(contents, "error")），扁平形态
                    # {"code":"1210"} 会被丢弃（raw_msg 尾部空格、归一失败落 agent.internal）
                    err_body = json.dumps(
                        {"error": {"code": "1210", "message": "messages.content.type 参数非法，取值范围 ['text']"}},
                        ensure_ascii=False, separators=(",", ":")).encode("utf-8")
                    self.send_header("Content-Length", str(len(err_body)))
                    self.end_headers()
                    self.wfile.write(err_body)
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")  # SSE 无 Content-Length，靠关闭定界
                self.close_connection = True
                self.end_headers()
                self.wfile.write(
                    b'data: {"id":"x","object":"chat.completion.chunk","choices":'
                    b'[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}\n\n'
                    b"data: [DONE]\n\n")

            def log_message(self, *args):  # 静默访问日志
                pass

        self.srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
        self.thread = threading.Thread(target=self.srv.serve_forever, daemon=True)

    def start(self):
        self.thread.start()

    def stop(self):
        self.srv.shutdown()

    def bodies(self):
        with self.lock:
            return [b for _, b in self.requests]

    def reset(self):
        with self.lock:
            self.requests = []


# ---------- DBPatch：模型配置直写 SQLite（model_configs + app_states） ----------

class DBPatch:
    """直写 data/sessions.db 的模型配置。库由 CLI 首启创建（脚本先启动一次再打补丁，
    规避自建 schema 与 Go 迁移逻辑的漂移）。全部操作幂等（INSERT OR REPLACE）。

    会写真实运行库——构造时备份 default_model_id 与测试模型行占用情况，restore()
    恢复原默认、清理测试模型与会话，退出必调（真实桌面端默认模型不可被冒烟改写）。"""

    def __init__(self, path, model_id):
        self.path = path
        self.model_id = model_id
        self.orig_default = None
        self.model_existed = False

    def _conn(self):
        return sqlite3.connect(self.path, timeout=10)

    def backup(self):
        conn = self._conn()
        try:
            row = conn.execute(
                "SELECT value FROM app_states WHERE app_name=? AND key=?",
                (APP_NAME, "default_model_id")).fetchone()
            self.orig_default = row[0] if row else None
            self.model_existed = conn.execute(
                "SELECT 1 FROM model_configs WHERE id=?", (self.model_id,)).fetchone() is not None
        finally:
            conn.close()
        return self.orig_default

    def restore(self, cleanup_sessions=True):
        conn = self._conn()
        try:
            if self.orig_default is not None:
                conn.execute(
                    "INSERT OR REPLACE INTO app_states(app_name,key,value) VALUES(?,?,?)",
                    (APP_NAME, "default_model_id", self.orig_default))
            else:
                conn.execute("DELETE FROM app_states WHERE app_name=? AND key=?",
                             (APP_NAME, "default_model_id"))
            if not self.model_existed:
                conn.execute("DELETE FROM model_configs WHERE id=?", (self.model_id,))
            if cleanup_sessions:
                conn.execute("DELETE FROM session_events WHERE session_id LIKE 'mm-smoke-%'")
                conn.execute("DELETE FROM sessions WHERE session_id LIKE 'mm-smoke-%'")
            conn.commit()
        finally:
            conn.close()

    def upsert_model(self, model_id, provider, image_support, base_url, full_url=1):
        now = int(time.time())
        conn = self._conn()
        try:
            conn.execute(
                "INSERT OR REPLACE INTO model_configs"
                "(id, provider, display_name, model_id, api_format, base_url, full_url,"
                " api_key, temperature, max_tokens, thinking, tool_rounds, image_support,"
                " created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (model_id, provider, "smoke", model_id, "openai", base_url, full_url,
                 "sk-smoke", 0.7, 0, 0, 1, 1 if image_support else 0, now, now))
            conn.commit()
        finally:
            conn.close()

    def set_default(self, model_id):
        conn = self._conn()
        try:
            conn.execute(
                "INSERT OR REPLACE INTO app_states(app_name, key, value) VALUES(?,?,?)",
                (APP_NAME, "default_model_id", model_id))
            conn.commit()
        finally:
            conn.close()

    def set_image_support(self, model_id, image_support):
        conn = self._conn()
        try:
            conn.execute("UPDATE model_configs SET image_support=?, updated_at=? WHERE id=?",
                         (1 if image_support else 0, int(time.time()), model_id))
            conn.commit()
        finally:
            conn.close()

    def read_events(self, session_id):
        """O6：直读会库该会话全部事件 JSON（升序）。"""
        conn = self._conn()
        try:
            rows = conn.execute(
                "SELECT seq, event FROM session_events"
                " WHERE app_name=? AND user_id=? AND session_id=? ORDER BY seq",
                (APP_NAME, USER_ID, session_id)).fetchall()
            return [(seq, json.loads(evt)) for seq, evt in rows]
        finally:
            conn.close()


# ---------- 基建（与 smoke_cli_l1.py 同源） ----------

def sse_ask(question, session_id, page_url, images=None, timeout=SSE_TIMEOUT):
    """POST /api/ask 并读完整 SSE 流。返回 (回答文本, error事件列表)。"""
    body = {"question": question, "sessionId": session_id, "page": {"url": page_url}}
    if images:
        body["images"] = images
    req = urllib.request.Request(BASE + "/api/ask", data=json.dumps(body).encode("utf-8"),
                                 headers={"Content-Type": "application/json"})
    chunks, errors = [], []
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        for raw in resp:
            line = raw.decode("utf-8", errors="replace").strip()
            if not line.startswith("data:"):
                continue
            payload = line[len("data:"):].strip()
            if not payload:
                continue
            try:
                evt = json.loads(payload)
            except json.JSONDecodeError:
                continue
            if evt.get("type") == "token":
                chunks.append(evt.get("content", ""))
            elif evt.get("type") == "error":
                errors.append(evt)
            if evt.get("done"):
                break
    return "\n".join(chunks), errors


def health_ok():
    try:
        with urllib.request.urlopen(BASE + "/health", timeout=3) as resp:
            return resp.status == 200
    except Exception:
        return False


def get_capabilities():
    """GET /api/capabilities → (status, parsed_json|None)。"""
    try:
        with urllib.request.urlopen(BASE + "/api/capabilities", timeout=5) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return e.code, None
    except Exception:
        return 0, None


class LogTailer:
    def __init__(self, path):
        self.path = path
        self.offset = os.path.getsize(path) if os.path.exists(path) else 0

    def read_new(self):
        if not os.path.exists(self.path):
            return ""
        size = os.path.getsize(self.path)
        if size < self.offset:  # 轮转/重建：从头读
            self.offset = 0
        with open(self.path, "r", encoding="utf-8", errors="replace") as f:
            f.seek(self.offset)
            text = f.read()
            self.offset = f.tell()
        return text


class CliProcess:
    def __init__(self):
        self.proc = None

    def start(self):
        if self.proc:
            return
        if health_ok():
            raise RuntimeError("7788 端口已被占用（疑似已有服务实例），请先停止后重试")
        self.proc = subprocess.Popen(
            [CLI_EXE, "--cli"], cwd=AGENT_DIR,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            creationflags=subprocess.CREATE_NEW_PROCESS_GROUP)
        deadline = time.time() + HEALTH_TIMEOUT
        while time.time() < deadline:
            if health_ok():
                return
            if self.proc.poll() is not None:
                raise RuntimeError("CLI 进程提前退出，exit=%s" % self.proc.returncode)
            time.sleep(0.5)
        raise RuntimeError("CLI %ds 内未通过 /health 探活" % HEALTH_TIMEOUT)

    def restart(self):
        self.stop()
        time.sleep(1)
        self.start()

    def stop(self):
        if self.proc and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
        self.proc = None


class ConfigPatch:
    """config.yaml 整文件备份/恢复（UTF-8 原生读写，保留全部注释）。"""

    def __init__(self):
        self.backup = None

    def apply(self, old_line, new_line):
        assert self.backup is None, "存在未恢复的补丁"
        with open(CONFIG_PATH, "r", encoding="utf-8") as f:
            self.backup = f.read()
        patched = self.backup.replace(old_line, new_line, 1) if old_line in self.backup else None
        if patched is None:  # 锚点未命中（可能已置空）：仅备份，不打补丁
            return
        with open(CONFIG_PATH, "w", encoding="utf-8", newline="") as f:
            f.write(patched)

    def restore(self):
        if self.backup is not None:
            with open(CONFIG_PATH, "w", encoding="utf-8", newline="") as f:
                f.write(self.backup)
            self.backup = None


# ---------- 断言 helpers ----------

def last_user_message(evt_json):
    """从事件 JSON 取 user 消息（含 content_parts）。

    事件 JSON 顶层为展平形态：event.Event 匿名内嵌 *model.Response，choices
    直接位于顶层（无 "response" 包裹），见会库实测。
    """
    try:
        msg = evt_json["choices"][0]["message"]
        if msg.get("role") == "user":
            return msg
    except (KeyError, IndexError, TypeError):
        pass
    return None


def has_image_part(msg, jpeg_only=False):
    """消息是否含 image part（jpeg_only=True 时仅统计 jpeg dataURL 形态）。"""
    for p in msg.get("content_parts", []) or []:
        if p.get("type") == "image" and p.get("image"):
            url = p["image"].get("url", "")
            if jpeg_only and not url.startswith("data:image/jpeg;base64,"):
                continue
            return url
    return None


class Runner:
    def __init__(self, report):
        self.report = report
        self.ok = True

    def check(self, cond, desc, detail=""):
        good = bool(cond)
        if not good:
            self.ok = False
        self.report("  [%s] %s%s" % ("PASS" if good else "FAIL", desc,
                                    ("；" + detail) if detail else ""))
        return good


def find_ask_body(bodies, marker):
    """主问答请求体锚定：最后一条 user 消息含当前轮提问原文。

    memory 提取器与摘要 worker 复用同一默认模型（假端点同样收到其异步请求），
    直接取 bodies[-1] 会被污染；主问答请求的最后一条 user 消息即当前轮完整
    prompt（含提问原文），memory 请求最后一条是提取指令——按此区分。
    返回 (body_bytes, parsed_json|None)，找不到返回 (None, None)。
    """
    for b in reversed(bodies):
        try:
            obj = json.loads(b)
        except Exception:
            continue
        msgs = obj.get("messages") or []
        if msgs and msgs[-1].get("role") == "user" and marker in str(msgs[-1].get("content", "")):
            return b, obj
    return None, None


# ---------- 用例 ----------

def case_live(runner, tag, want):
    """LIVE：capabilities 探活——200 + 字段存在 + 取值符合当前默认模型配置。"""
    status, data = get_capabilities()
    runner.check(status == 200, "[%s] GET /api/capabilities 返回 200" % tag, "实际 %d" % status)
    runner.check(isinstance(data, dict) and "imageSupport" in data,
                 "[%s] 响应含 imageSupport 字段" % tag, "实际 %s" % (data,))
    if want is not None and isinstance(data, dict):
        runner.check(data.get("imageSupport") == want,
                     "[%s] imageSupport == %s（随 DBPatch 翻转）" % (tag, want),
                     "实际 %s" % data.get("imageSupport"))


def case_mm1(runner, fake, png_big):
    """MM1：1210 只提示不重试——带图请求恰好 1 次（按带图请求计数，避开 memory 异步污染）。"""
    fake.mode = "1210"
    fake.reset()
    sid = "mm-smoke-1210-%d" % int(time.time() * 1000)
    answer, errors = sse_ask("描述这张图", sid, SMOKE_P, images=[
        {"dataUrl": png_big, "mime": "image/png"}])
    runner.check(len(errors) == 1 and errors[0].get("code") == "model.image_unsupported",
                 "MM1 SSE 唯一 error 且 code=model.image_unsupported",
                 "errors=%s" % json.dumps(errors, ensure_ascii=False)[:200])
    fake.mode = "ok"
    with_image = [b for b in fake.bodies() if b'"image"' in b or b"image_url" in b]
    runner.check(len(with_image) == 1,
                 "MM1 带图请求恰好 1 次（只提示不重试，无剥图重发）",
                 "实际 %d 次" % len(with_image))
    return sid


def read_stored_user_image(session_id, dbpatch):
    """O6：从会库读该会话 user 事件的 image part（返回 (url|None, 全部user消息数)）。"""
    events = dbpatch.read_events(session_id)
    url, n_user = None, 0
    for _, evt in events:
        msg = last_user_message(evt)
        if msg:
            n_user += 1
            u = has_image_part(msg)
            if u:
                url = u
    return url, n_user


def case_mm2(runner, fake, dbpatch, tailer, png_big):
    """MM2：缩略图生命周期——原图进请求 → 落库缩略 → 同 scope 重放 / 换 scope L1 注入。"""
    fake.mode = "ok"
    fake.reset()
    sid = "mm-smoke-life-%d" % int(time.time() * 1000)

    # 第 1 轮：发图（原图）
    answer, errors = sse_ask("描述这张图", sid, SMOKE_P, images=[
        {"dataUrl": png_big, "mime": "image/png"}])
    runner.check(not errors, "MM2 第1轮 SSE 无错误", str(errors)[:150])
    time.sleep(1.0)  # 等 runner 收尾落库（done 后仍 drain 至 append 完成）

    # O1：当轮请求体为原图（PNG dataURL 原样发出，未被预先降采样）
    first_bodies = [b for b in fake.bodies() if png_big.encode() in b]
    runner.check(len(first_bodies) >= 1,
                 "MM2 O1 当轮请求体携带原图（服务端不预处理当轮图）",
                 "含原图请求 %d 次" % len(first_bodies))

    # O6：落库为 jpeg 缩略 <10KB
    url, _ = read_stored_user_image(sid, dbpatch)
    runner.check(url is not None and url.startswith("data:image/jpeg;base64,"),
                 "MM2 O6 落库 image part 为 jpeg dataURL（方案 A 降级）",
                 "实际 %.40s" % (url or "None"))
    runner.check(url is not None and len(url) < THUMB_MAX_BYTES,
                 "MM2 O6 落库缩略 <10KB（%d 阈值）" % THUMB_MAX_BYTES,
                 "实际 %d 字节" % (len(url) if url else -1))

    # 第 2 轮：同 scope 追问 → SDK scope 重放携带缩略 part
    tailer.read_new()
    _, errors = sse_ask("图里的主要内容是什么？", sid, SMOKE_P)
    runner.check(not errors, "MM2 第2轮（同scope重放）SSE 无错误", str(errors)[:150])
    time.sleep(0.5)
    replay, _ = find_ask_body(fake.bodies(), "图里的主要内容是什么？")
    runner.check(replay is not None, "MM2 O1 锚定到第2轮主问答请求体")
    if replay is not None:
        runner.check(b"data:image/jpeg;base64," in replay,
                     "MM2 同 scope 重放请求含 jpeg 缩略 part")
        runner.check(png_big.encode() not in replay,
                     "MM2 同 scope 重放不含原图（原图不回流上下文）")

    # 第 3 轮：换 scope 追问 → L1 注入缩略 part + 文本存在性标记
    tailer.read_new()
    _, errors = sse_ask("换个话题，但结合之前发过的图说说", sid, SMOKE_Q)
    runner.check(not errors, "MM2 第3轮（跨scope L1）SSE 无错误", str(errors)[:150])
    time.sleep(0.5)
    log = tailer.read_new()
    l1 = [tuple(int(x) for x in m.groups()) for m in RE_L1.finditer(log)]
    runner.check(len(l1) >= 1 and l1[-1][1] >= 1,
                 "MM2 O2 跨 scope 轮 L1 注入（kept>=1）", "L1 行=%s" % (l1[-2:] or l1,))
    l1_body, _ = find_ask_body(fake.bodies(), "换个话题，但结合之前发过的图")
    runner.check(l1_body is not None, "MM2 O1 锚定到第3轮主问答请求体")
    if l1_body is not None:
        runner.check(b"data:image/jpeg;base64," in l1_body,
                     "MM2 L1 注入请求含 jpeg 缩略 part")
        l1_text = l1_body.decode("utf-8", errors="replace")
        runner.check("[本轮含 1 张图片" in l1_text,
                     "MM2 L1 注入含文本存在性标记（随文本流进 L2/L3 摘要）")
    return sid


def case_mm3(runner, fake, sid):
    """MM3：非视觉门控防回归——同 scope 追问请求体不含 image part。
    前置（调用方完成）：默认模型 image_support 已切 0 且 CLI 已重启。"""
    fake.reset()
    _, errors = sse_ask("之前那张图的背景是什么颜色？", sid, SMOKE_P)
    runner.check(not errors, "MM3 追问 SSE 无错误（非视觉模型不因库内缩略图 1210）",
                 str(errors)[:150])
    time.sleep(0.5)
    body, _ = find_ask_body(fake.bodies(), "之前那张图的背景是什么颜色")
    runner.check(body is not None, "MM3 O1 锚定到主问答请求体")
    if body is not None:
        runner.check(b"image_url" not in body and b"data:image" not in body,
                     "MM3 请求体不含任何 image part（WithTextOnlyMessageContent 剥离 + L1 门控）")


# ---------- 主流程 ----------

def main():
    ts = datetime.now().strftime("%Y%m%d-%H%M%S")
    report_path = os.path.join(HERE, "smoke_report_mm_%s.txt" % ts)
    rep_file = open(report_path, "w", encoding="utf-8")

    def report(s=""):
        print(s)
        rep_file.write(s + "\n")
        rep_file.flush()

    results = {}
    patch = ConfigPatch()
    cli = CliProcess()
    fake = FakeEndpoint(FAKE_PORT)
    dbpatch = DBPatch(DB_PATH, "mm-smoke-m1")
    tailer = LogTailer(LOG_PATH)
    png_big = make_png(800, 600)
    report("PNG 原图体积: %d 字节" % len(png_big))

    try:
        report("=== 多模态协议冒烟 %s ===" % ts)
        report("CLI: %s" % CLI_EXE)
        report("假端点: %s (mode 切换)" % FAKE_URL)
        report("会库: %s" % DB_PATH)
        if not os.path.exists(CLI_EXE):
            report("[FATAL] CLI exe 不存在，请先 make build-cli")
            return 2

        try:
            patch.apply(PROXY_LINE, PROXY_EMPTY)
            report("代理已临时置空（直连假端点）\n")
        except AssertionError as e:
            report("[WARN] config 补丁失败: %s\n" % e)

        fake.start()

        # 启动 #1：建库（含 config.yaml 模型迁移，探活只断言端点存活与字段存在）
        cli.start()
        dbpatch.backup()  # 备份真实 default_model_id（退出时恢复，桌面端默认模型不可被冒烟改写）
        report("启动 #1（建库）\n--- LIVE 探活（端点存活） ---")
        live1 = Runner(report)
        case_live(live1, "启动1", want=None)
        results["LIVE 探活(启动1)"] = live1.ok
        cli.stop()

        # DBPatch：视觉能力开启的假端点模型设为默认 → 启动 #2
        dbpatch.upsert_model("mm-smoke-m1", "bigmodel", True, FAKE_URL)
        dbpatch.set_default("mm-smoke-m1")
        cli.start()
        report("\n--- LIVE 探活（值翻转 true） ---")
        live2 = Runner(report)
        case_live(live2, "启动2", want=True)
        results["LIVE 翻转(true)"] = live2.ok

        report("\n--- MM1 1210 只提示不重试 ---")
        r1 = Runner(report)
        results["MM1 1210只提示"] = case_mm1(r1, fake, png_big)

        report("\n--- MM2 缩略图生命周期 ---")
        r2 = Runner(report)
        sid_life = case_mm2(r2, fake, dbpatch, tailer, png_big)
        results["MM2 缩略图生命周期"] = r2.ok

        # MM3：切非视觉 → 重启 → 探活翻转 → 追问（切库必须在重启前）
        dbpatch.set_image_support("mm-smoke-m1", False)
        report("\n--- MM3 非视觉门控防回归 ---")
        cli.restart()
        report("--- LIVE 探活（值翻转 false） ---")
        live3 = Runner(report)
        case_live(live3, "重启3", want=False)
        results["LIVE 翻转(false)"] = live3.ok
        r3 = Runner(report)
        case_mm3(r3, fake, sid_life)
        results["MM3 非视觉门控"] = r3.ok

    except Exception as e:
        report("  [FAIL] 脚本异常: %r" % e)
        report(traceback.format_exc(limit=6))
        results["脚本异常"] = False
    finally:
        try:
            patch.restore()
        except Exception:
            pass
        try:
            cli.stop()
        except Exception:
            pass
        try:
            dbpatch.restore()  # 恢复原默认模型 + 清理冒烟模型与会话（真实运行库复位）
        except Exception:
            pass
        fake.stop()
        report("\n=== 汇总 ===")
        npass = sum(1 for ok in results.values() if ok)
        for name, ok in results.items():
            report("  %s  %s" % ("PASS" if ok else "FAIL", name))
        report("通过 %d / %d" % (npass, len(results)))
        report("config.yaml 已恢复；报告保留: %s" % report_path)
        rep_file.close()
    return 0 if npass == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
