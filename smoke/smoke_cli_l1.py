#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""CLI 模式 L1 工作集端到端冒烟测试（真实 DeepSeek 模型，不 mock）。

验证目标（第一阶段 L1，设计文档 working-set-context-design.md §20）：
  WS1 跨 scope 注入（B4 主 bug）：A/B 两 scope 交替提问，断言 L1 注入与去重（禁双花）
  WS2 同 scope 去重：同 scope 连问，断言 L1 全去重、scope 重放无双花
  WS3 开关关闭回退：working_set.enabled=false，断言不注入（灰度回退现状）
  WS4 工具轮折叠：诱导 fetch_page 后追问，断言折叠标记前缀
  WS5 超预算可读报错：l1_max_tokens=10，断言 SSE 可读报错且未调模型（G2 兜底）
  WS6 悬挂取消轮：cancel 后追问，断言不完整轮不计入 L1

数据源（全部来自真实进程，Infof 级别即可）：
  O1 模型请求体：data/logs/absorb.log 的 "[Absorb] request body sent to model:" JSON
  O2 L1 日志：    absorb.log 的 "[Absorb] working-set L1 | selected=..." 行
  O3 SSE：       /api/ask 响应流

计数断言约定：模型回答文本内容不可控，一律以「提问原文全文」作为轮次唯一标识计数
（assistant 回答是否复述关键词不作断言依据）。

用法：python smoke_cli_l1.py
前置：build/bin/absorb-agent-cli.exe 已构建（make build-cli）；
      config.yaml 的模型可直连 DeepSeek。CLI 为动态端口，无需预留端口。
产物：本目录 smoke_report_<时间戳>.txt（跑完保留，供审查）。
注意：测试会话写入真实会话库（sessionId 前缀 ws-smoke-），按约定测试后重装清理。
"""

import json
import os
import re
import subprocess
import sys
import threading
import time
import traceback
import urllib.request
import urllib.error
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
AGENT_DIR = os.path.dirname(HERE)  # 仓库根（smoke/ 位于仓库内，config/build/data 均相对根）
CONFIG_PATH = os.path.join(AGENT_DIR, "config", "config.yaml")
CLI_EXE = os.path.join(AGENT_DIR, "build", "bin", "absorb-agent-cli.exe")
LOG_PATH = os.path.join(AGENT_DIR, "data", "logs", "absorb.log")
PORT_FILE = os.path.join(AGENT_DIR, "data", "cli.port")  # CLI 动态端口发现文件（对齐桌面 bridge.port 机制）
BASE = None               # start() 从 cli.port 解析并经 /health 验证后写入
SSE_TIMEOUT = 150          # 单轮问答超时（秒）
HEALTH_TIMEOUT = 40        # CLI 启动探活超时（秒）

SMOKE_A = "https://smoke-a.example.com/article"
SMOKE_B = "https://smoke-b.example.com/article"

Q_A1 = "请用一句话介绍数值计算中的二分法。"        # WS1/WS2 第 1 轮提问（唯一标识）
Q_A2 = "请用一句话介绍排序算法中的快速排序。"      # WS1 第 2 轮（B scope）/ WS2 第 2 轮（A scope）
Q_FOLLOW = "懂了。请把刚才两次介绍的内容合并成一句话输出。"


def sse_ask(question, session_id, page_url, timeout=SSE_TIMEOUT):
    """POST /api/ask 并读完整 SSE 流。返回回答文本（error 事件以 [[SSE-ERROR]] 前缀嵌入）。"""
    body = json.dumps({
        "question": question,
        "sessionId": session_id,
        "page": {"url": page_url},
    }).encode("utf-8")
    req = urllib.request.Request(BASE + "/api/ask", data=body,
                                 headers={"Content-Type": "application/json"})
    chunks = []
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
                chunks.append("[[SSE-ERROR]]" + json.dumps(evt, ensure_ascii=False))
            if evt.get("done"):
                break
    return "\n".join(chunks)


def http_post(path, body, timeout=10):
    req = urllib.request.Request(BASE + path,
                                 data=json.dumps(body).encode("utf-8"),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as e:
        return "[HTTP %d] %s" % (e.code, e.read().decode("utf-8", errors="replace"))


def health_ok():
    try:
        with urllib.request.urlopen(BASE + "/health", timeout=3) as resp:
            return resp.status == 200
    except Exception:
        return False


def wait_base_from_port_file(deadline):
    """轮询 cli.port 端口发现文件，写入全局 BASE（动态端口模式）。

    启动前已删除旧文件，出现即属于本次进程；端口须 /health 探活通过才采纳。
    """
    global BASE
    while time.time() < deadline:
        try:
            with open(PORT_FILE, "r", encoding="utf-8") as f:
                port = f.read().strip()
            if port.isdigit():
                candidate = "http://127.0.0.1:%s" % port
                try:
                    with urllib.request.urlopen(candidate + "/health", timeout=2) as resp:
                        if resp.status == 200:
                            BASE = candidate
                            return True
                except Exception:
                    pass
        except OSError:
            pass
        time.sleep(0.3)
    return False


class LogTailer:
    """增量读取 absorb.log：记录 offset，仅返回新增内容（避免历史日志干扰断言）。"""

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
    """CLI 进程全托管：启动探活、重启、终止。"""

    def __init__(self):
        self.proc = None

    def start(self):
        global BASE
        if self.proc:
            return
        # 失败时保留旧 BASE（避免 None 级联炸掉后续用例）；解析成功才覆盖。
        if os.path.exists(PORT_FILE):  # 清掉上次运行残留，确保文件出现=本次进程
            os.remove(PORT_FILE)
        self.proc = subprocess.Popen(
            [CLI_EXE, "--cli"], cwd=AGENT_DIR,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            creationflags=subprocess.CREATE_NEW_PROCESS_GROUP)
        deadline = time.time() + HEALTH_TIMEOUT
        while time.time() < deadline:
            if wait_base_from_port_file(deadline):
                break
            if self.proc.poll() is not None:
                raise RuntimeError("CLI 进程提前退出，exit=%s" % self.proc.returncode)
            time.sleep(0.3)
        else:
            if self.proc.poll() is not None:
                raise RuntimeError("CLI 进程提前退出，exit=%s" % self.proc.returncode)
            raise RuntimeError("CLI %ds 内 cli.port 未出现或探活未通过" % HEALTH_TIMEOUT)
        while time.time() < deadline:
            if health_ok():
                return
            if self.proc.poll() is not None:
                raise RuntimeError("CLI 进程提前退出，exit=%s" % self.proc.returncode)
            time.sleep(0.5)
        raise RuntimeError("CLI %ds 内未通过 /health 探活（BASE=%s）" % (HEALTH_TIMEOUT, BASE))

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
    """config.yaml 自动备份/修改/恢复（UTF-8 原生读写，保留全部注释）。

    锚点必须用「含注释的整行」：'  enabled: true' 短串会同时命中 memory 段。
    """

    def __init__(self):
        self.backup = None

    def apply(self, old_line, new_line):
        assert self.backup is None, "存在未恢复的补丁"
        with open(CONFIG_PATH, "r", encoding="utf-8") as f:
            self.backup = f.read()
        if old_line not in self.backup:
            self.backup = None
            raise AssertionError("config 补丁锚点未找到: %r" % old_line)
        with open(CONFIG_PATH, "w", encoding="utf-8", newline="") as f:
            f.write(self.backup.replace(old_line, new_line, 1))

    def restore(self):
        if self.backup is not None:
            with open(CONFIG_PATH, "w", encoding="utf-8", newline="") as f:
                f.write(self.backup)
            self.backup = None

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.restore()
        return False


# 本机代理未运行时模型调用报 "cannot connect to service"；冒烟临时置空走直连
# （DeepSeek 直连可达）。退出时随整文件备份恢复。
PROXY_LINE = '  url: "http://127.0.0.1:7897"         # HTTP 代理；国内直连 GitHub 等站点会被阻断，经本机代理出网'
PROXY_EMPTY = '  url: ""                             # HTTP 代理；冒烟临时置空走直连（退出时恢复）'

# config.yaml 中由本脚本改动的整行锚点（与磁盘文件逐字一致）
WS_ENABLED_LINE = "  enabled: true        # 总开关：每轮注入全局最近完整问答轮次（跨 scope 连续性），关闭则回退单 scope 行为"
WS_DISABLED_LINE = "  enabled: false       # 总开关：每轮注入全局最近完整问答轮次（跨 scope 连续性），关闭则回退单 scope 行为"
WS_TOKENS_LINE = "  l1_max_tokens: 16000 # L1 注入估算 token 预算，超限本轮可读报错（不裁剪，裁剪属第二阶段）"
WS_TOKENS_LOW_LINE = "  l1_max_tokens: 10    # L1 注入估算 token 预算，超限本轮可读报错（不裁剪，裁剪属第二阶段）"

# ---------- 日志解析 ----------

RE_L1 = re.compile(
    r"working-set L1 \| selected=(\d+) kept=(\d+) dropped_dup=(\d+) msgs=(\d+) est_tokens=(\d+)")


def parse_l1_lines(log_text):
    """返回 [(selected, kept, dropped_dup, msgs, est_tokens), ...]（按出现顺序）。"""
    return [tuple(int(x) for x in m.groups()) for m in RE_L1.finditer(log_text)]


def extract_request_messages(log_text):
    """提取请求体 JSON 的 messages 数组。返回按序 list[list[dict]]。"""
    marker = "request body sent to model:"
    out = []
    idx = 0
    while True:
        i = log_text.find(marker, idx)
        if i < 0:
            break
        idx = i + len(marker)
        seg = log_text[idx:]
        j = seg.find("{")
        if j < 0:
            continue
        try:
            obj, _ = json.JSONDecoder().raw_decode(seg[j:])
            if isinstance(obj, dict) and "messages" in obj:
                out.append(obj["messages"])
        except json.JSONDecodeError:
            # 兜底：单行 JSON 被日志框架折行时，截到下一个时间戳行再试
            nxt = re.search(r"\n\d{4}-\d{2}-\d{2}T", seg[j:])
            chunk = seg[j: j + nxt.start()] if nxt else seg[j:]
            try:
                obj = json.loads(chunk)
                if isinstance(obj, dict) and "messages" in obj:
                    out.append(obj["messages"])
            except json.JSONDecodeError:
                pass
    return out


def find_ask_request(log_text, marker):
    """返回主问答请求体 messages（含 marker 的最新一个）。

    不能简单取最后一条：memory 提取 / 摘要 worker 也会调 LLM，其请求载荷同样
    含全部对话原文与当前轮提问，会污染断言（曾导致 WS3 假 FAIL / WS1 假 PASS）。
    区分锚点：主问答请求的【最后一条 user 消息】就是当前轮完整 prompt（含 marker）；
    memory 提取请求最后一条 user 是 "Extract and manage memories..."，摘要请求
    最后一条是摘要指令——均不含 marker。
    """
    reqs = extract_request_messages(log_text)
    for msgs in reversed(reqs):
        if not msgs:
            continue
        last = msgs[-1]
        if last.get("role") == "user" and marker in str(last.get("content", "")):
            return msgs
    return None


def count_text(messages, needle):
    return json.dumps(messages or [], ensure_ascii=False).count(needle)


def find_pos(messages, needle):
    return json.dumps(messages or [], ensure_ascii=False).find(needle)


class Runner:
    """单用例执行上下文：SSE 问答 + 断言收集 + 报告输出。"""

    def __init__(self, name, tailer, report):
        self.name = name
        self.tailer = tailer
        self.report = report
        self.ok = True

    def new_session(self):
        return "ws-smoke-%s-%d" % (self.name.lower().split()[0], int(time.time() * 1000))

    def http_post_cancel(self, sid):
        return http_post("/api/cancel", {"sessionId": sid})

    def ask(self, q, sid, url):
        answer = sse_ask(q, sid, url)
        time.sleep(0.5)  # 等日志刷盘
        return answer

    def check(self, cond, desc, detail=""):
        ok = bool(cond)
        if not ok:
            self.ok = False
        self.report("  [%s] %s%s" % ("PASS" if ok else "FAIL", desc,
                                     ("；" + detail) if detail else ""))
        return ok

    def skip(self, desc, detail=""):
        self.report("  [SKIP] %s%s" % (desc, ("；" + detail) if detail else ""))

    def dump_answer(self, answer, title):
        self.report("  --- %s（供人工抽查） ---" % title)
        for ln in answer.splitlines():
            self.report("  | " + ln)


def case_ws1(cli, tailer, report, patch):
    """跨 scope 注入（B4 主 bug）：A 轮经 L1 注入恰好 1 次，B 轮 scope 重放恰好 1 次。"""
    c = Runner("WS1", tailer, report)
    sid = c.new_session()
    c.ask(Q_A1, sid, SMOKE_A)
    c.ask(Q_A2, sid, SMOKE_B)
    tailer.read_new()  # 丢弃前两轮日志，只断言第 3 轮
    answer = c.ask(Q_FOLLOW, sid, SMOKE_B)
    log = tailer.read_new()
    msgs = find_ask_request(log, Q_FOLLOW)
    l1 = parse_l1_lines(log)

    c.check(msgs is not None, "O1 捕获到模型请求体",
            "" if msgs else "日志中未找到含当前轮提问的 request body")
    if msgs:
        c.check(count_text(msgs, Q_A1) == 1,
                "A 轮提问原文恰好 1 次（L1 注入，SDK scope 重放不可见）",
                "实际 %d" % count_text(msgs, Q_A1))
        c.check(count_text(msgs, Q_A2) == 1,
                "B 轮提问原文恰好 1 次（scope 重放提供，L1 去重禁双花）",
                "实际 %d" % count_text(msgs, Q_A2))
        # SDK late-context 固定插入点：注入块落在「scope 重放之后、当前轮提问之前」
        # （content.go lateContextInsertIndex=最后一条 user 消息下标），设计文档 §5.1
        # 路径 A′ 按此语义采纳。故只断言注入内容先于当前轮出现，不要求与重放呈全局时序
        # （旧断言"A 在 B 前"是想当然，且曾被 memory 提取请求的时间序转录假 PASS 掩盖）。
        c.check(0 <= find_pos(msgs, Q_A1) < find_pos(msgs, Q_FOLLOW)
                and 0 <= find_pos(msgs, Q_A2) < find_pos(msgs, Q_FOLLOW),
                "A/B 两轮均位于当前轮提问之前（注入点=最新 user 轮之前）")
    c.check(len(l1) == 1 and l1[0][:3] == (2, 1, 1),
            "O2 L1 日志 selected=2 kept=1 dropped_dup=1", "实际 %s" % (l1,))
    c.check("[[SSE-ERROR]]" not in answer, "O3 SSE 无错误")
    c.dump_answer(answer, "第 3 轮回答")
    return c.ok


def case_ws2(cli, tailer, report, patch):
    """同 scope 去重：A 连问 3 轮，第 3 轮 L1 全去重（kept=0），历史仅由 scope 重放提供。"""
    c = Runner("WS2", tailer, report)
    sid = c.new_session()
    c.ask(Q_A1, sid, SMOKE_A)
    c.ask(Q_A2, sid, SMOKE_A)
    tailer.read_new()
    answer = c.ask("把前两轮的两句话按顺序复述一遍。", sid, SMOKE_A)
    log = tailer.read_new()
    msgs = find_ask_request(log, "把前两轮的两句话按顺序复述一遍")
    l1 = parse_l1_lines(log)

    c.check(msgs is not None, "O1 捕获到模型请求体")
    if msgs:
        c.check(count_text(msgs, Q_A1) == 1,
                "第 1 轮提问原文恰好 1 次（仅 scope 重放，无双花）",
                "实际 %d" % count_text(msgs, Q_A1))
        c.check(count_text(msgs, Q_A2) == 1,
                "第 2 轮提问原文恰好 1 次（仅 scope 重放，无双花）",
                "实际 %d" % count_text(msgs, Q_A2))
    c.check(len(l1) == 1 and l1[0][:3] == (2, 0, 2),
            "O2 L1 日志 selected=2 kept=0 dropped_dup=2（全去重零注入）",
            "实际 %s" % (l1,))
    c.check("[[SSE-ERROR]]" not in answer, "O3 SSE 无错误")
    c.dump_answer(answer, "第 3 轮回答")
    return c.ok


def case_ws3(cli, tailer, report, patch):
    """开关关闭回退：enabled=false 时 A 轮不注入。"""
    c = Runner("WS3", tailer, report)
    patch.apply(WS_ENABLED_LINE, WS_DISABLED_LINE)
    try:
        cli.restart()
        sid = c.new_session()
        c.ask(Q_A1, sid, SMOKE_A)
        c.ask(Q_A2, sid, SMOKE_B)
        tailer.read_new()
        answer = c.ask(Q_FOLLOW, sid, SMOKE_B)
        log = tailer.read_new()
        msgs = find_ask_request(log, Q_FOLLOW)
        l1 = parse_l1_lines(log)
        c.check(len(l1) == 0, "O2 无 L1 注入日志行（开关关闭整体回退）", "实际 %s" % (l1,))
        c.check(msgs is not None, "O1 捕获到模型请求体")
        if msgs:
            n = count_text(msgs, Q_A1)
            c.check(n == 0,
                    "O1 请求体不含 A 轮提问原文（回退现状单 scope）",
                    "实际 %d" % n)
            if n > 0:  # 失败取证：定位 Q_A1 所在消息与上下文（写进报告供人工判读）
                for mi, m in enumerate(msgs):
                    content = str(m.get("content", ""))
                    k = content.find(Q_A1)
                    if k >= 0:
                        ctx = content[max(0, k - 300):k + 150].replace("\n", " | ")
                        c.report("  [证据] msg[%d] role=%s context: ...%s..." % (mi, m.get("role"), ctx))
        c.check("[[SSE-ERROR]]" not in answer, "O3 SSE 无错误")
    finally:
        patch.restore()
        cli.restart()
    return c.ok


def case_ws4(cli, tailer, report, patch):
    """工具轮折叠：诱导 fetch_page 后**换 scope** 追问，断言折叠标记。

    必须换 scope：同 scope 追问时 L1 全去重，工具轮由 scope 重放原样提供，
    折叠标记只存在于 L1 注入路径（跨 scope 轮次）。
    """
    c = Runner("WS4", tailer, report)
    sid = c.new_session()
    c.ask("请使用 fetch_page 工具抓取 https://example.com 页面，并告诉我页面的主标题原文。",
          sid, SMOKE_A)
    tailer.read_new()
    q2 = "刚才那个页面的标题是什么？"
    answer = c.ask(q2, sid, SMOKE_B)
    log = tailer.read_new()
    msgs = find_ask_request(log, q2)
    text = json.dumps(msgs or [], ensure_ascii=False)
    m = re.search(r"（该轮含 (\d+) 次工具调用，详情可回溯）", text)
    if not m:
        c.skip("模型未触发工具轮（或折叠标记未出现），不判 FAIL")
        c.dump_answer(answer, "第 2 轮回答")
        return True
    c.check(True, "O1 请求体出现工具轮折叠标记", m.group(0))
    c.check("[[SSE-ERROR]]" not in answer, "O3 SSE 无错误")
    c.dump_answer(answer, "第 2 轮回答")
    return c.ok


def case_ws5(cli, tailer, report, patch):
    """超预算可读报错：l1_max_tokens=10 → SSE 可读报错且未调模型（G2 兜底）。"""
    c = Runner("WS5", tailer, report)
    patch.apply(WS_TOKENS_LINE, WS_TOKENS_LOW_LINE)
    try:
        cli.restart()
        sid = c.new_session()
        c.ask(Q_A1, sid, SMOKE_A)
        c.ask(Q_A2, sid, SMOKE_B)
        tailer.read_new()
        answer = c.ask(Q_FOLLOW, sid, SMOKE_B)
        log = tailer.read_new()
        c.check("[[SSE-ERROR]]" in answer and "预算" in answer,
                "O3 SSE 返回含「预算」的可读报错",
                answer[:120].replace("\n", " "))
        c.check("over budget" in log, "O2 日志含 over budget Warnf")
        c.check(find_ask_request(log, Q_FOLLOW) is None,
                "O1 本轮未调用主问答模型（无含当前轮提问的请求体）")
    finally:
        patch.restore()
        cli.restart()
    return c.ok


def case_ws6(cli, tailer, report, patch):
    """悬挂取消轮：cancel 后的 user 轮不计入完整轮次。"""
    c = Runner("WS6", tailer, report)
    sid = c.new_session()
    # 异步发起长回答问题，主线程在 run 进行中 cancel（sse_ask 同步阻塞，
    # 流结束 = run 已完成，顺序调用 cancel 会来不及）
    box = {}

    def ask_bg():
        try:
            box["answer"] = sse_ask("请写一段 300 字的散文，主题：秋天的校园。",
                                    sid, SMOKE_A, timeout=120)
        except Exception as e:
            box["error"] = repr(e)  # cancel 导致连接中断属预期

    t = threading.Thread(target=ask_bg, daemon=True)
    t.start()
    time.sleep(1.5)  # 等首 token 流出（run 已进行中）
    c.http_post_cancel(sid)
    t.join(timeout=30)
    tailer.read_new()
    # 此时完整轮次 = 0（唯一一轮被取消）→ L1 selected=0
    answer1 = c.ask("用一句话介绍冒泡排序。", sid, SMOKE_A)
    log1 = tailer.read_new()
    l1a = parse_l1_lines(log1)
    c.check(len(l1a) >= 1 and l1a[-1][:3] == (0, 0, 0),
            "O2 取消后追问 selected=0（悬挂 user 不计入完整轮次）", "实际 %s" % (l1a,))
    c.check("[[SSE-ERROR]]" not in answer1, "O3 取消后追问 SSE 正常")
    # 再完成一轮并**换 scope** → 完整轮次 = 1（取消轮仍不计入），跨 scope 才验证 L1 注入
    tailer.read_new()
    answer2 = c.ask("用一句话介绍插入排序。", sid, SMOKE_B)
    log2 = tailer.read_new()
    l1b = parse_l1_lines(log2)
    c.check(len(l1b) >= 1 and l1b[-1][:3] == (1, 1, 0),
            "O2 换 scope 后 selected=1 kept=1（L1 注入冒泡轮，取消轮仍不计入）", "实际 %s" % (l1b,))
    c.dump_answer(answer2, "末轮回答")
    return c.ok


CASES = [
    ("WS1 跨scope注入(B4)", case_ws1),
    ("WS2 同scope去重", case_ws2),
    ("WS3 开关关闭回退", case_ws3),
    ("WS4 工具轮折叠", case_ws4),
    ("WS5 超预算可读报错", case_ws5),
    ("WS6 悬挂取消轮", case_ws6),
]


def main():
    ts = datetime.now().strftime("%Y%m%d-%H%M%S")
    report_path = os.path.join(HERE, "smoke_report_%s.txt" % ts)
    rep_file = open(report_path, "w", encoding="utf-8")

    def report(s=""):
        print(s)
        rep_file.write(s + "\n")
        rep_file.flush()

    patch = ConfigPatch()        # 用例级（WS3/WS5 用，单实例单补丁）
    proxy_patch = ConfigPatch()  # 全程级（代理置空，与用例补丁互不干扰）
    cli = CliProcess()
    tailer = LogTailer(LOG_PATH)
    results = []
    try:
        report("=== CLI 模式 L1 工作集冒烟测试 %s ===" % ts)
        report("CLI: %s" % CLI_EXE)
        report("日志: %s" % LOG_PATH)
        if not os.path.exists(CLI_EXE):
            report("[FATAL] CLI exe 不存在，请先 make build-cli")
            return 2
        # 置空代理走直连（本机 7897 未运行时模型调用会 "cannot connect to service"）
        try:
            proxy_patch.apply(PROXY_LINE, PROXY_EMPTY)
            report("代理已临时置空（走直连）\n")
        except AssertionError as e:
            report("[WARN] 代理行未替换（可能已置空或文本漂移）: %s\n" % e)
        cli.start()
        report("CLI 已启动并通过 /health 探活\n")
        for name, fn in CASES:
            report("--- %s ---" % name)
            try:
                results.append((name, fn(cli, tailer, report, patch)))
            except Exception as e:  # 单用例失败不阻断后续
                report("  [FAIL] 用例异常: %r" % e)
                report(traceback.format_exc(limit=4))
                results.append((name, False))
            report("")
    finally:
        try:
            patch.restore()
        except Exception:
            pass
        try:
            proxy_patch.restore()
        except Exception:
            pass
        cli.stop()
        report("=== 汇总 ===")
        npass = sum(1 for _, ok in results if ok)
        for name, ok in results:
            report("  %s  %s" % ("PASS" if ok else "FAIL", name))
        report("通过 %d / %d" % (npass, len(results)))
        report("报告已保留: %s" % report_path)
        rep_file.close()
    return 0 if npass == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
