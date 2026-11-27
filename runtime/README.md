# runtime — 随包分发的离线运行环境

## 组成

| 目录 | 内容 | 体积 |
|---|---|---|
| `mingit/` | 裁剪版 Git for Windows（POSIX shell + coreutils），供 `workspace_exec` 的 `sh` 使用 | ~17.8MB |
| `python/` | 嵌入式 Python 3.12（stdlib-only，无 pip），供技能脚本的 `python3` 使用 | ~21.4MB |
| `poppler/` | poppler-windows 构建（pdftotext + 依赖 DLL + share/poppler 数据），供本地 PDF 提取 | ~34.7MB |

两个目录均为零安装、纯便携。agent 启动时自动探测（见 `config/skillconf.go`），命中后前置注入进程 PATH：
系统已有可用的 sh/bash/python 则优先用系统；缺失（或 `bash` 是 WSL 启动器桩、`python3` 是 Microsoft Store 桩）时用捆绑版兜底。
poppler 的探测不同：不进 PATH，由 `extract.pdftotextBin()` 按「ABSORB_PDFTOTEXT 环境变量 → exe 旁 runtime/poppler → PATH」顺序解析（见 `internal/document/extract/poppler.go`）。

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

---

# runtime/poppler — 捆绑 PDF 文本提取引擎（poppler-windows）

## 用途

本地 file:// PDF 的提取引擎（本地文件不走 httpfetch 主路，poppler 是唯一引擎，
见 `internal/document/extract/localfile.go` / `ExtractLocal`）；远程 PDF 的双跑
择优第二引擎也复用同一查找逻辑。`share/poppler/` 是 CID/unicode 映射数据
（缺内嵌 ToUnicode 的中文 PDF 依赖它），`PopplerText` 调用时自动以
`POPPLER_DATADIR` 注入捆绑数据目录。

## 来源与版本

- 上游：oschwartz10612/poppler-windows 官方构建（conda-forge 打包的 Windows 版 poppler）
- 下载地址：https://github.com/oschwartz10612/poppler-windows/releases/tag/v24.08.0-0
- 文件：`Release-24.08.0-0.zip`（x64）
- 版本：24.08.0（pdftotext -v 输出确认）

## 目录结构约定

`pdftotext.exe` 必须与本目录直连的 `share/poppler/` 同在（`PopplerText` 按
「bin 旁 share/poppler」探测注入 datadir），依赖 DLL 与 pdftotext.exe 同目录
（Windows 加载器从 exe 自身目录解析依赖），整目录搬运、不可只取单个 exe。

## 更新约定

1. 下载新版 `Release-<ver>-0.zip`
2. 删除本目录，解压 zip 后把 `poppler-<ver>/Library/bin/` 全量与
   `poppler-<ver>/share/poppler/` 复制为 `runtime/poppler/{*.exe,*.dll,share/poppler/}`
3. 验证 `pdftotext.exe -v` 可执行、对 docs/pdf 样书提取非空
4. 更新本文档版本号与下载地址
5. 跑 `go test ./internal/document/ -run TestPrepareLocalFileReady`（pdftotext 可用时不跳过）确认链路

## 许可证

poppler 遵循 **GPL-2.0-or-later**（及部分组件 AGPL）；闭源商业分发需附带许可证
文本（`share/poppler/COPYING*` 已随包保留）并按要求提供对应源码
（源码见 https://poppler.freedesktop.org/ 与构建仓库
https://github.com/oschwartz10612/poppler-windows ）。
