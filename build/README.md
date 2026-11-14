# build/ — Wails 构建资源（源资产，纳入版本控制）

本目录不是构建产物目录。`wails build` 从这里读取资源嵌入 exe，路径由 Wails CLI 约定，不可自定义。

| 文件 | 作用 |
|---|---|
| `appicon.png` | 应用图标源文件，构建时生成各平台尺寸 |
| `windows/icon.ico` | exe / 安装器图标 |
| `windows/info.json` | 版本信息（嵌入 exe 元数据） |
| `windows/wails.exe.manifest` | exe 清单（DPI 感知、权限声明） |

**产物目录是 `build/bin/`**（exe / 安装包），已被 `.gitignore` 忽略。
请勿把整个 `build/` 加入忽略列表——那会丢掉上述源资产，其他机器将无法完整构建。

与 `assets/appicon.png` 的关系：同一张图、两个消费方。本目录的供 Wails 构建期使用，
`assets/` 的经 `go:embed` 编译进二进制供运行时托盘图标使用（见 embed.go）。
