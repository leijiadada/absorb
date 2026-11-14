# releases/ — 升级 feed

`latest.json` 是客户端升级检查的数据源（默认地址见 `internal/conf` 的
`DefaultUpgradeFeedURL`，即本文件在 master 上的 raw 地址）。

协议与字段含义见 [design/upgrade.md](../design/upgrade.md) 第六节。

## 维护方式（GitHub Actions 自动化）

**由发布流水线生成，不要手改。** 打 tag 即完成发版全流程
（[.github/workflows/release.yml](../.github/workflows/release.yml)）：

1. tag 须打在 master HEAD 上：`git tag -a vX.Y.Z -m "..." && git push github vX.Y.Z`；
2. 流水线自动完成：版本元数据同步 → 前端构建 → 扩展 bundle → wails 桌面端 +
   CLI exe → Inno Setup 安装包 → 扩展 zip → genfeed 生成 `latest.json`；
3. 安装包与扩展 zip 上传到 GitHub Release（tag `vX.Y.Z`）；
4. 流水线把更新后的 `latest.json` 自动提交回 master（`[skip ci]`）；
5. 如需覆盖存量客户端（其内置 feed 地址仍指向 Gitee raw），本地把该提交
   同步推送到 Gitee（`git push origin master`）即可。

本地手工兜底路径（CI 不可用时）：`make installer VERSION=X.Y.Z` → 上传
`build/bin/absorb-agent-setup-X.Y.Z.exe` 到 Release → `go run ./cmd/genfeed
-version X.Y.Z -root .` → 提交推送。

字段约束（客户端会校验，不合法即视为坏 feed）：

- `version` 必须可解析为 semver（不带 `-` 预发布时最稳）；
- `channel` 非空；
- `artifacts` 至少一项，`url` 必须 https，`sha256` 为 64 位十六进制；
- `minimum_version`（可缺省）：低于该版本的客户端会被标记为**强制升级**。
