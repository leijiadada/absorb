#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""摘要 Report 可观测验证：ctx 穿线 + WithReportHook 结构化日志。

问题背景（修复前）：
  store.SessionService.EnqueueSummaryJob 的 ctx 参数被丢弃（`_ context.Context`），
  SDK runner 入队时放进 ctx 的请求作用域资产（summaryview 视图 / cache-safe fork
  请求 / report 管线）全部无法到达摘要生成层。本修复：worker 以 WithoutCancel +
  agent.CloneContext 穿线（复刻 SDK async.go DetachContext 形状），并给摘要器
  配置 WithReportHook，把 SDK Report（触发归因 + 摘要模型调用记账）落结构化日志。

数据源（全部来自真实进程 absorb.log）：
  O1 摘要 Report 行： "[Absorb] summary report | filterKey=... trigger=... mode=..." （新增）
  O2 摘要生成行：     "[Absorb] session summary generated ... filterKey=..." （回归锚点）
  O3 SSE：           /api/ask 响应流

判定（全部命中即通过）：
  RP-1 Report 日志行出现，trigger=manual fired=true（无 checks 形态的恒 fire 归因）
  RP-2 mode=cache_safe_fork（fork 主路径：ctx 携带父请求快照）且 prompt_tokens>0
      （standalone 回退形态 mode=standalone 由单测覆盖，不在真机冒烟强制复现）
  RP-3 行尾 err=<nil>（摘要成功无错误）
  RP-4 session summary generated 行仍正常（filterKey 为 16 位十六进制，回归锚点）
  RP-5 SSE 无错误

用法：python smoke_report.py
前置：build/bin/absorb-agent-cli.exe 已构建；7788 端口空闲；
      config.yaml 的 deepseek-flash 可直连（脚本临时置空代理，退出恢复）。
产物：本目录 smoke_report_report_<时间戳>.txt。
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
SUMMARY_WAIT = 120  # 摘要生成等待上限（秒）

Q_TOOL = "请调用 fetch_page 工具抓取 https://example.com ，并用一句话说明该页面的主标题。"

CONFIG_PATCH = "\n\n# --- report smoke patch (auto, restore on exit) ---\nsummary:\n  event_threshold: 2\n"

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


# ---------- 日志解析 ----------

# Report 行：ctx 穿线 + WithReportHook 的新增结构化日志（trigger/mode/tokens）。
# 必须 MULTILINE：tailer 按增量块读取，$ 需锚定块内每行行尾而非块尾
# （否则 report 行后紧跟其他日志行时永久失配——真机首跑曾因此误报 FAIL）。
RE_REPORT = re.compile(
    r"summary report \| filterKey=\"([^\"]*)\" trigger=(\S+) fired=(true|false) "
    r"metric=(\S+) value=(\d+) threshold=(\d+) mode=(\S+) prompt_tokens=(\d+) "
    r"est_prompt_tokens=(\d+) cached_tokens=(\d+) err=(.+)$", re.MULTILINE)
# 回归锚点：摘要生成日志（filterKey 为 16 位十六进制，方案乙标识）
RE_SUMMARY = re.compile(
    r"session summary generated \(session=(\S+) filterKey=\"([0-9a-f]{16})\" url=\"([^\"]*)\" events=(\d+)")


def parse_report_lines(log_text):
    """返回 [(filterKey, trigger, fired, metric, value, threshold, mode,
    prompt_tokens, est_prompt_tokens, cached_tokens, err), ...]"""
    out = []
    for m in RE_REPORT.finditer(log_text):
        out.append((m.group(1), m.group(2), m.group(3), m.group(4),
                    int(m.group(5)), int(m.group(6)), m.group(7),
                    int(m.group(8)), int(m.group(9)), int(m.group(10)),
                    m.group(11).strip()))
    return out


def parse_summary_lines(log_text):
    return [(m.group(1), m.group(2), m.group(3), int(m.group(4)))
            for m in RE_SUMMARY.finditer(log_text)]


def main():
    ts = datetime.now().strftime("%Y%m%d-%H%M%S")
    report_path = os.path.join(HERE, "smoke_report_report_%s.txt" % ts)
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
    sid = "report-smoke-%d" % int(time.time() * 1000)

    try:
        report("=== 摘要 Report 可观测验证：ctx 穿线 + WithReportHook %s ===" % ts)
        report("CLI: %s" % CLI_EXE)
        report("日志: %s" % LOG_PATH)
        report("会话: %s" % sid)
        report("")
        if not os.path.exists(CLI_EXE):
            report("[FATAL] CLI exe 不存在，请先构建 build-cli")
            return 2

        patch.apply()
        cli.start()
        report("CLI 已启动并通过 /health 探活（config 已临时追加 summary.event_threshold=2）\n")

        # 步骤1：一轮带工具链的提问（4+ 事件 > 阈值 2，诱发异步摘要）
        report("--- 步骤1：fetch_page 工具链提问（诱发摘要） ---")
        a1 = sse_ask(Q_TOOL, sid, "https://report-smoke.example.com/article")
        time.sleep(0.5)
        check("[[SSE-ERROR]]" not in a1, "RP-5 SSE 无错误", a1[:120].replace("\n", " "))

        # 步骤2：等摘要 Report 与生成日志落盘（异步 worker）。
        # 匹配结果跨轮累积：tailer 按增量块读取，单个块可能只含其一，
        # 覆盖式赋值会把上一轮已命中的结果清掉（真机首跑曾因此误报 FAIL）。
        report("--- 步骤2：等待摘要 Report 日志（上限 %ds） ---" % SUMMARY_WAIT)
        deadline = time.time() + SUMMARY_WAIT
        reports = []
        summaries = []
        while time.time() < deadline:
            new = tailer.read_new()
            reports.extend(parse_report_lines(new))
            summaries.extend(parse_summary_lines(new))
            if reports and summaries:
                break
            time.sleep(2)

        check(len(reports) >= 1, "RP-1 摘要 Report 日志行已出现（WithReportHook 生效）",
              "reports=%s" % (reports[-2:],))
        if reports:
            r = reports[-1]
            check(r[1] == "manual" and r[2] == "true",
                  "RP-1b trigger=manual fired=true（无 checks 形态的恒 fire 归因）",
                  "实际 trigger=%s fired=%s" % (r[1], r[2]))
            check(r[6] == "cache_safe_fork",
                  "RP-2a mode=cache_safe_fork（fork 主路径：父请求快照经 ctx 穿线到达）",
                  "实际 mode=%s" % r[6])
            check(r[7] > 0 or r[8] > 0,
                  "RP-2b prompt_tokens/est_prompt_tokens 非零（调用记账生效）",
                  "实际 prompt_tokens=%d est=%d" % (r[7], r[8]))
            check(r[10] == "<nil>",
                  "RP-3 err=<nil>（摘要成功无错误）", "实际 err=%s" % r[10])
        check(len(summaries) >= 1,
              "RP-4 session summary generated 行仍正常（回归锚点，filterKey 16 位十六进制）",
              "summaries=%s" % (summaries[-2:],))

        report("")
        if state["ok"]:
            report("结论：Report 可观测生效——触发归因与调用记账已结构化落日志，"
                   "既有摘要链路回归正常。")
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
