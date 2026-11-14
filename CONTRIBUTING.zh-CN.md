# 贡献指南

[English](CONTRIBUTING.md) | **中文**

感谢你愿意参与 Absorb 阅读助手。本文件说明如何提 issue、提交 PR、搭建开发环境与代码规范。

## 行为准则

请保持友善、就事论事。我们欢迎建设性的讨论与批评。

## 如何开始

1. 提 issue：bug、功能建议或设计讨论，请先搜索是否已有相关 issue。
2. 认领 / 讨论：在 issue 中说明你的打算，避免重复劳动。

## 开发环境

依赖：Go 1.26+、Node.js 24+、[Wails v2 CLI](https://wails.io/docs/gettingstarted/installation)、Windows 10/11（WebView2 Runtime）。

```bash
make build      # 构建桌面端完整产物（build/bin）
make build-cli  # 构建 CLI 控制台版
make run        # 构建并启动桌面端
make test       # Go 单元测试 + 前端/扩展 vitest 全量测试
```

调试扩展链路可用 `make run-cli` 启动前台 CLI 服务模式。

## 代码规范

- Go：遵循 `gofmt` / `go vet`；新增逻辑优先放入对应的 `internal/` 子模块。
- 前端 / 扩展：Vue 3 + Vite + Tailwind，组件与脚本需有 vitest 覆盖。
- 提交信息：用清晰的中文或英文描述「做了什么、为什么」。

## 设计决策

产品定义、演进路线与吸收闭环方案在 `design/` 目录（`orientation.md`、`ld.md`）。涉及用户流程或数据模型的改动，请先对齐设计文档再动手。

## PR 流程

1. Fork 并新建分支。
2. 确保 `make test` 通过。
3. 提交 PR，描述动机、改动与测试方式；关联相关 issue。
