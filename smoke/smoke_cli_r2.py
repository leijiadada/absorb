#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""R2 修复态验证：scope 标识定长哈希（方案乙）+ 真实 DeepSeek 模型 + 日志断言。

问题背景（修复前的 R2）：
  父页面 P 与子页面 C 的 URL 存在 "/" 层级。C 触发摘要后回到 P 提问，SDK 前缀
  聚合注入 C 的摘要、cutoff 按全会话事件下标一刀切，P 的 cutoff 前历史被剔除；
  L1 去重又把 P 轮当"分支内重复"丢弃 → 请求中 P 的历史完全消失。

修复（方案乙）：filterKey 不用裸 URL，改用归一后的定长十六进制哈希
  （storage.ScopeID）。标识不含 "/" 且互不为前缀，SDK 的分支过滤/摘要前缀
  聚合/scope 过滤全部退化为精确匹配：
    - 聚合永不触发（R2-2 修复态：摘要标记 0 次）；
    - 无 cutoff，P 的历史由 SDK 全量重放（R2-3 修复态：父轮提问恰好 1 次）；
    - C 轮作为跨 scope 轮次由 L1 注入（R2-4 修复态：selected=2 kept=1）；
    - 人类可读网址另存 scope_urls 映射表，日志双写（filterKey + url）。

数据源（全部来自真实进程 absorb.log）：
  O1 模型请求体："[Absorb] request body sent to model:" JSON
  O2 L1 日志：    "[Absorb] working-set L1 | selected=..." 行
  O3 摘要生成：   "[Absorb] session summary generated ... filterKey=... url=..." 行
  O4 scope 打标： "[Absorb] summary scope filterKey=... url=..." 行
  O5 SSE：       /api/ask 响应流

判定（修复态预期，全部命中即通过；括号内为修复前复现态供对照）：
  R2-1 子 scope 摘要已生成，日志 filterKey 为 16 位十六进制、url 为子页面归一网址
  R2-2 请求体不含子页面摘要（前缀聚合已不可能）（复现态：注入 1 次）
  R2-3 父 scope 首轮提问恰好 1 次（历史不再丢失）（复现态：0 次）
  R2-4 L1 selected=2 kept=1 dropped_dup=1（C 轮跨 scope 注入，P 轮去重禁双花）（复现态：kept=0）
  R2-5 SSE 无错误

用法：python smoke_cli_r2.py
前置：build/bin/absorb-agent-cli.exe 已构建（go build -tags desktop,production）；
      7788 端口空闲；config.yaml 的 deepseek-flash 可经本机代理直连。
产物：本目录 smoke_report_r2_<时间戳>.txt。
注意：方案乙切换后，旧会话（裸 URL 打标）与本轮哈希键不相等——开发阶段重装可避。
"""

import json
import os
import re
import subprocess
import sys
import time
import traceback
import urllib.request
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
AGENT_DIR = os.path.dirname(HERE)  # 仓库根（smoke/ 位于仓库内，config/build/data 均相对根）
CONFIG_PATH = os.path.join(AGENT_DIR, "config", "config.yaml")
CLI_EXE = os.path.join(AGENT_DIR, "build", "bin", "absorb-agent-cli.exe")
LOG_PATH = os.path.join(AGENT_DIR, "data", "logs", "absorb.log")
BASE = "http://127.0.0.1:7788"
SSE_TIMEOUT = 180
HEALTH_TIMEOUT = 40
SUMMARY_WAIT = 120  # 子 scope 摘要生成等待上限（秒）

# 父子层级 URL（镜像真实文档站：章节目录页 → 子文档页）
SMOKE_P = "https://smoke-parent.example.com/docs/03-features"
SMOKE_C = "https://smoke-parent.example.com/docs/03-features/04-chunking"

Q_P1 = "请用一句话介绍数值计算中的二分法。"                    # 父 scope 第 1 轮（唯一标识）
Q_C1 = "请调用 fetch_page 工具抓取 https://example.com ，并用一句话说明该页面的主标题。"  # 子 scope 第 1 轮（诱导工具链）
Q_P2 = "这个章节除了二分法还涉及什么？请结合前面提到过的内容回答。"  # 父 scope 最终轮（断言锚点）

SUMMARY_MARK = "以上为压缩摘要"  # 摘要 worker 追加的后缀，证明摘要文本进入请求体

CONFIG_PATCH = "\n\n# --- R2 smoke patch (auto, restore on exit) ---\nsummary:\n  event_threshold: 2\n"

# 本机代理未运行时模型调用报 "cannot connect to service"；冒烟临时置空走直连
# （DeepSeek 直连可达，已用 Invoke-WebRequest -Proxy $null 验证 401=可达）。
PROXY_LINE = '  url: "http://127.0.0.1:7897"         # HTTP 代理；国内直连 GitHub 等站点会被阻断，经本机代理出网'
PROXY_EMPTY = '  url: ""                             # HTTP 代理；冒烟临时置空走直连（退出时恢复）'


def sse_ask(question, session_id, page_url, timeout=SSE_TIMEOUT):
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


def health_ok():
    try:
        with urllib.request.urlopen(BASE + "/health", timeout=3) as resp:
            return resp.status == 200
    except Exception:
        return False


class LogTailer:
    def __init__(self, path):
        self.path = path
        self.offset = os.path.getsize(path) if os.path.exists(path) else 0

    def read_new(self):
        if not os.path.exists(self.path):
            return ""
        size = os.path.getsize(self.path)
        if size < self.offset:
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
            raise RuntimeError("7788 端口已被占用，请先停止运行中的 absorb-agent 实例")
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
    """config.yaml 整文件备份；apply 在末尾追加 summary 段，restore 原样写回。"""

    def __init__(self):
        self.backup = None

    def apply(self):
        assert self.backup is None, "存在未恢复的补丁"
        with open(CONFIG_PATH, "r", encoding="utf-8") as f:
            self.backup = f.read()
        assert "summary:" not in self.backup, "config.yaml 已存在 summary 段，请先移除再跑本脚本"
        patched = self.backup + CONFIG_PATCH
        if PROXY_LINE in patched:
            patched = patched.replace(PROXY_LINE, PROXY_EMPTY, 1)
        with open(CONFIG_PATH, "w", encoding="utf-8", newline="") as f:
            f.write(patched)

    def restore(self):
        if self.backup is not None:
            with open(CONFIG_PATH, "w", encoding="utf-8", newline="") as f:
                f.write(self.backup)
            self.backup = None


# ---------- 日志解析（与 smoke_cli_l1.py 同源） ----------

RE_L1 = re.compile(
    r"working-set L1 \| selected=(\d+) kept=(\d+) dropped_dup=(\d+) msgs=(\d+) est_tokens=(\d+)")
# 摘要生成日志：filterKey 为 16 位十六进制（方案乙），url 为归一网址（人读）
RE_SUMMARY = re.compile(
    r"session summary generated \(session=(\S+) filterKey=\"([0-9a-f]{16})\" url=\"([^\"]*)\" events=(\d+)")
# scope 打标日志（handler 双写）
RE_SCOPE = re.compile(
    r"summary scope filterKey=\"([0-9a-f]{16})\" url=\"([^\"]*)\" \(session=(\S+)\)")


def parse_l1_lines(log_text):
    return [tuple(int(x) for x in m.groups()) for m in RE_L1.finditer(log_text)]


def parse_summary_lines(log_text):
    """返回 [(session, filterKey, url, events), ...]（修复态日志格式）。"""
    return [(m.group(1), m.group(2), m.group(3), int(m.group(4)))
            for m in RE_SUMMARY.finditer(log_text)]


def parse_scope_lines(log_text):
    """返回 [(filterKey, url, session), ...]（handler scope 打标双写日志）。"""
    return [(m.group(1), m.group(2), m.group(3)) for m in RE_SCOPE.finditer(log_text)]


def extract_request_messages(log_text):
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
    """主问答请求体：最后一条 user 消息含当前轮提问原文，且整个请求不是摘要/记忆请求。

    摘要 worker 的请求体只有一条 user 消息（摘要提示词 + 对话原文），对话原文里
    同样包含当前轮提问，仅凭 marker 会误抓（R2 首跑即踩中）；用摘要提示词前缀排除。
    """
    for msgs in reversed(extract_request_messages(log_text)):
        if not msgs:
            continue
        blob = json.dumps(msgs, ensure_ascii=False)
        if "你是会话摘要助手" in blob or "You are a Memory Manager" in blob:
            continue
        last = msgs[-1]
        if last.get("role") == "user" and marker in str(last.get("content", "")):
            return msgs
    return None


def count_text(messages, needle):
    return json.dumps(messages or [], ensure_ascii=False).count(needle)


def dump_messages(report, msgs, limit=90):
    report("  --- 模型实际收到的 messages（role | 内容前 %d 字） ---" % limit)
    for i, m in enumerate(msgs or []):
        content = str(m.get("content", "")).replace("\n", " | ")
        report("  | msg[%d] %-9s %s" % (i, m.get("role"), content[:limit]))


def main():
    ts = datetime.now().strftime("%Y%m%d-%H%M%S")
    report_path = os.path.join(HERE, "smoke_report_r2_%s.txt" % ts)
    rep_file = open(report_path, "w", encoding="utf-8")

    def report(s=""):
        print(s)
        rep_file.write(s + "\n")
        rep_file.flush()

    state = {"ok": True}

    def check(cond, desc, detail=""):
        good = bool(cond)
        if not good:
            state["ok"] = False
        report("  [%s] %s%s" % ("PASS" if good else "FAIL", desc,
                                 ("；" + detail) if detail else ""))

    patch = ConfigPatch()
    cli = CliProcess()
    tailer = LogTailer(LOG_PATH)
    sid = "r2-smoke-%d" % int(time.time() * 1000)

    try:
        report("=== R2 修复态验证：scope 标识定长哈希（方案乙） %s ===" % ts)
        report("CLI: %s" % CLI_EXE)
        report("日志: %s" % LOG_PATH)
        report("会话: %s" % sid)
        report("父 scope P: %s" % SMOKE_P)
        report("子 scope C: %s" % SMOKE_C)
        report("")
        if not os.path.exists(CLI_EXE):
            report("[FATAL] CLI exe 不存在，请先 go build -tags desktop,production "
                   "-o build/bin/absorb-agent-cli.exe .")
            return 2

        patch.apply()
        cli.start()
        report("CLI 已启动并通过 /health 探活（config 已临时追加 summary.event_threshold=2）\n")

        # 步骤1：父 scope 一轮（2 事件，不触发 P 摘要）
        report("--- 步骤1：父 scope P 提问（不触发摘要） ---")
        a1 = sse_ask(Q_P1, sid, SMOKE_P)
        time.sleep(0.5)
        check("[[SSE-ERROR]]" not in a1, "步骤1 SSE 无错误", a1[:120].replace("\n", " "))
        tailer.read_new()

        # 步骤2：子 scope 一轮带工具链（4 事件 > 2，触发 C 摘要）
        report("--- 步骤2：子 scope C 提问（诱导 fetch_page，事件数超阈值） ---")
        a2 = sse_ask(Q_C1, sid, SMOKE_C)
        time.sleep(0.5)
        check("[[SSE-ERROR]]" not in a2, "步骤2 SSE 无错误", a2[:120].replace("\n", " "))

        # 步骤3：等 C 的摘要真正落库（异步 worker）
        report("--- 步骤3：等待子 scope 摘要生成（上限 %ds） ---" % SUMMARY_WAIT)
        deadline = time.time() + SUMMARY_WAIT
        summaries = []
        while time.time() < deadline:
            summaries = parse_summary_lines(tailer.read_new())
            if any(u == SMOKE_C for _, _, u, _ in summaries):
                break
            time.sleep(2)
        c_sums = [x for x in summaries if x[2] == SMOKE_C]
        p_sums = [x for x in summaries if x[2] == SMOKE_P]
        check(len(c_sums) >= 1, "R2-1 子 scope 摘要已生成，且日志 url= 为子页面归一网址（标识为 16 位十六进制）",
              "summaries=%s" % (summaries[-3:],))
        check(len(p_sums) == 0, "R2-1b 父 scope 未生成自己的摘要（聚合路径前提）",
              "P summaries=%s" % (p_sums,))
        if c_sums:
            fk = c_sums[-1][1]
            check(re.fullmatch(r"[0-9a-f]{16}", fk) is not None,
                  "R2-1c 摘要日志 filterKey 为 16 位十六进制（方案乙标识）", "实际 %r" % fk)
        if not c_sums:
            report("  [中止] 前提不满足（C 未生成摘要），后续断言无意义。"
                   "请检查步骤2 是否真的诱发了工具调用（事件数需 >2）。")
            dump_messages(report, find_ask_request(tailer.read_new(), Q_C1))
            return 1

        # 步骤4：回到父 scope 提问（断言轮）
        report("--- 步骤4：回到父 scope P 提问（断言轮） ---")
        tailer.read_new()
        a3 = sse_ask(Q_P2, sid, SMOKE_P)
        time.sleep(1.0)
        log = tailer.read_new()
        msgs = find_ask_request(log, Q_P2)
        l1 = parse_l1_lines(log)
        scope_lines = parse_scope_lines(log)

        check("[[SSE-ERROR]]" not in a3, "R2-5 SSE 无错误（静默丢失，用户侧无感知）",
              a3[:120].replace("\n", " "))
        check(msgs is not None, "O1 捕获到断言轮模型请求体")
        if msgs:
            n_sum = count_text(msgs, SUMMARY_MARK)
            check(n_sum == 0,
                  "R2-2 请求体不含子页面摘要（前缀聚合已不可能；复现态为注入 1 次）",
                  "标记出现 %d 次" % n_sum)
            n_p1 = count_text(msgs, Q_P1)
            check(n_p1 == 1,
                  "R2-3 父 scope 首轮提问恰好 1 次（历史不再丢失；复现态为 0 次）",
                  "实际 %d" % n_p1)
            n_c1 = count_text(msgs, Q_C1)
            check(n_c1 == 1,
                  "R2-3b 子 scope 轮次经 L1 跨 scope 注入恰好 1 次（连续性保持、无双花）",
                  "实际 %d" % n_c1)
            dump_messages(report, msgs)
        check(len(l1) >= 1 and l1[-1][:3] == (2, 1, 1),
              "R2-4 L1 selected=2 kept=1 dropped_dup=1（C 轮跨 scope 注入，P 轮去重禁双花；复现态 kept=0）",
              "实际 %s" % (l1,))
        # 日志双写：handler 打标行必须同时含 16 位标识与归一网址
        p_scope = [x for x in scope_lines if x[1] == SMOKE_P]
        check(len(p_scope) >= 1 and re.fullmatch(r"[0-9a-f]{16}", p_scope[-1][0]) is not None,
              "R2-6 handler scope 打标日志双写（标识+网址），标识为 16 位十六进制",
              "实际 %s" % (scope_lines[-3:],))

        report("")
        report("--- 断言轮回答（供人工抽查） ---")
        for ln in a3.splitlines():
            report("  | " + ln)

        report("")
        if state["ok"]:
            report("结论：R2 已修复——父 scope 历史完整重放（恰好 1 次），子页面摘要不再被"
                   "错误聚合，子 scope 轮次经 L1 注入保持连续性，标识/网址日志双写可追溯。")
        else:
            report("结论：存在未命中项（见上方 FAIL），需人工判读。")
    except Exception as e:
        report("  [FAIL] 脚本异常: %r" % e)
        report(traceback.format_exc(limit=6))
        state["ok"] = False
    finally:
        try:
            patch.restore()
        except Exception:
            pass
        cli.stop()
        report("")
        report("config.yaml 已恢复；报告保留: %s" % report_path)
        rep_file.close()
    return 0 if state["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
