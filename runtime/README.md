# runtime — 随包分发的离线运行环境

## 组成

| 目录 | 内容 | 体积 |
|---|---|---|
| `mingit/` | 裁剪版 Git for Windows（POSIX shell + coreutils），供 `workspace_exec` 的 `sh` 使用 | ~17.8MB |
| `python/` | 嵌入式 Python 3.12（stdlib-only，无 pip），供技能脚本的 `python3` 使用 | ~21.4MB |

两个目录均为零安装、纯便携。agent 启动时自动探测（见 `config/skillconf.go`），命中后前置注入进程 PATH：
系统已有可用的 sh/bash/python 则优先用系统；缺失（或 `bash` 是 WSL 启动器桩、`python3` 是 Microsoft Store 桩）时用捆绑版兜底。

---

# runtime/mingit — 捆绑 POSIX Shell（MinGit）

## 用途

框架固定 spawn 两个 POSIX shell：
- `sh`：`workspace_exec` 执行技能脚本命令
- `bash`：工作区元数据提交、技能运行（`internal/skillstage` / `tool/skill`）

Windows 上 `sh` 仅来自 Git Bash（机器无 Git Bash 时无法解析）；更隐蔽的是 `bash`——
`C:\Windows\System32\bash.exe`（WSL 启动器）在 PATH 中优先于 Git Bash，未安装 WSL 发行版时
任何 `bash` 调用直接 exit 1（这正是 workspace_exec reconcile 失败的根因）。

此目录提供随包分发的 MinGit（Git for Windows 官方嵌入式最小版），agent 启动时自动探测
（见 `config/skillconf.go` 的 `systemShellOK`/`firstBundledShellDir`），命中后将其
`usr/bin` 目录前置注入进程 PATH，实现零安装执行技能脚本。

## 来源与版本

- 上游：Git for Windows 官方 Release（MinGit 为嵌入式/第三方应用设计的发行版）
- 下载地址：https://github.com/git-for-windows/git/releases/tag/v2.55.0.windows.5
- 文件：`MinGit-2.55.0.5-64-bit.zip`（x64；ARM64 机器请换 `MinGit-<ver>-arm64.zip`）
- 国内镜像（GitHub 直连不通时）：https://mirrors.tuna.tsinghua.edu.cn/github-release/git-for-windows/git/
- 版本：2.55.0.5（Git for Windows 2026-08-20 发布）

## 更新约定

1. 下载新版 `MinGit-<ver>-64-bit.zip`
2. 删除本目录，将新 zip 解压为 `runtime/mingit/`
3. **裁剪**（本运行时只承担 POSIX shell + coreutils，不需要 git/ssh/GUI）：
   - 删除 `mingw64/`（git 本体 + Git Credential Manager + Skia/Avalonia 等 GUI 组件，约 63MB）
   - 删除 `cmd/`（git 启动器）
   - 删除 `usr/bin/` 下的 `ssh.exe`、`ssh-add.exe`、`ssh-agent.exe`、`rebase.exe`、`msys-sqlite3-0.dll`、`msys-crypto-3.dll`
   - 删除 `usr/lib/ssh/`
   - 若脚本需要 git/ssh，应保留对应部分（当前技能集不需要）
4. **生成 bash shim**：`Copy-Item usr/bin/sh.exe usr/bin/bash.exe`（MinGit 自带 sh 即 bash，
   复制后以 `bash` 名调用即为全功能 bash；框架元数据提交固定用 `bash`，漏了它 workspace_exec 会 reconcile 失败）
5. 验证 `usr/bin/sh.exe` 与 `usr/bin/bash.exe` 均存在，且 `sh -lc "echo ok"`、`bash -lc "echo ok"` 可执行
6. 更新本文档版本号与下载地址
7. 跑 `go test ./config/ -run TestBundledShellEndToEnd` 确认端到端链路

## 许可证

MinGit 随 Git for Windows 分发，遵循 **GPL-2.0** 许可证；闭源商业分发需附带本说明
及许可证文本（zip 根目录 `LICENSE.txt` 已随包保留），并按要求提供对应源码
（源码位于 https://gitforwindows.org/ 的 sources 下载区）。

---

# runtime/python — 捆绑嵌入式 Python

## 用途

技能脚本（如 diagram-design）按 `python3 <skill-dir>/scripts/xxx.py` 调用，
机器未安装 Python 或 `python3` 被 Microsoft Store 桩（App Execution Alias）占据时无法执行。
此目录提供嵌入式 Python 发行版（embeddable，仅解释器 + 标准库），随包分发、零安装。

## 来源与版本

- 上游：python.org 官方嵌入式发行版（embeddable package）
- 下载地址：https://www.python.org/ftp/python/3.12.3/python-3.12.3-embed-amd64.zip
- 国内镜像（python.org 直连不通时）：https://registry.npmmirror.com/-/binary/python/3.12.3/python-3.12.3-embed-amd64.zip
- 版本：3.12.3（与开发机系统 Python 一致）

## 关键说明

- **`python3.exe` 是 `python.exe` 的副本**：技能文档统一用 `python3` 调用，
  嵌入式包只带 `python.exe`，故需复制一份 shim，更新时勿漏。
- **无 pip / 无 site-packages**：纯 stdlib 技能（diagram-design 4 个脚本）可正常跑；
  若未来技能需要第三方库（如 Pillow），需另行处理，不能依赖本包。
- **无 tkinter/GUI 模块**：无头执行不受影响。

## 更新约定

1. 下载新版 `python-3.12.x-embed-amd64.zip`
2. 删除本目录，解压为新版
3. 执行 `Copy-Item python.exe python3.exe` 生成 shim
4. 验证 `python3 -c "print(1)"` 可执行，且技能脚本所需 stdlib 可导入
5. 更新本文档版本号与下载地址
6. 跑 `go test ./config/ -run TestBundledPython` 确认端到端链路

## 许可证

Python 3.x 遵循 **PSF License Agreement**（PSF 2.0 兼容），允许再分发，需附带许可证文本
（zip 内 `LICENSE.txt` 已随包保留）。
