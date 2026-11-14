# Absorb 阅读助手 —— 构建入口

APP       := absorb-agent-desktop
APP_CLI   := absorb-agent-cli
BIN_DIR   := build/bin
# 命令行可覆盖: make installer VERSION=0.2.0
VERSION   ?= 0.1.0
# 评测入口可覆盖: make eval SET=knowledge-relevance RUNS=2
SET       ?= understanding-basic
RUNS      ?= 3
# Go 工具链默认走 PATH；工具链不在 PATH 的环境（如 msys64 mingw 构建）在 Makefile.local 覆盖
GO        ?= go
GOFMT     ?= gofmt
# lint 工具不在 PATH 时在 Makefile.local 覆盖为绝对路径（安装：go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest 与 go install golang.org/x/vuln/cmd/govulncheck@latest）
GOLANGCI  ?= golangci-lint
GOVULN    ?= govulncheck
# Wails CLI（官方构建管线）；不在 PATH 的环境在 Makefile.local 覆盖为绝对路径
WAILS     ?= wails
# msys64 登录 shell（-l）的 /etc/profile 默认 minimal 档只保留系统 PATH 段，
# 会丢用户 PATH（npm/node 所在）；inherit 让登录环境继承完整 Windows PATH。
# 非 msys 环境此变量无消费者、无副作用。
export MSYS2_PATH_TYPE := inherit
# 本机覆盖（gitignored，勿提交）
-include Makefile.local
PS        := powershell.exe -NoProfile -Command
# ISCC 默认走 ensure-iscc.ps1 的全量探测（Inno Setup 7/6 + 用户目录安装），
# 仍可用 make installer ISCC=C:/path/ISCC.exe 人工指定
ISCC ?= $(shell powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ensure-iscc.ps1 -Resolve)
EXE_PATH  := $(BIN_DIR)/$(APP).exe

.DEFAULT_GOAL := build
.PHONY: build build-cli build-eval build-web pack-assets sync-version ensure-iscc installer release test test-go test-web test-extension-e2e vet lint govuln eval fmt run run-cli clean

NM_STAMP := frontend/node_modules/.absorb-install-stamp
$(NM_STAMP): frontend/package.json frontend/package-lock.json
	@$(PS) "cd frontend; npm ci; if (\$$LASTEXITCODE -ne 0) { exit 1 }; New-Item -ItemType File -Path 'node_modules\.absorb-install-stamp' -Force | Out-Null"

E2E_STAMP := e2e/node_modules/.absorb-install-stamp
$(E2E_STAMP): e2e/package.json e2e/package-lock.json
	@$(PS) "cd e2e; npm ci; if (\$$LASTEXITCODE -ne 0) { exit 1 }; New-Item -ItemType File -Path 'node_modules\.absorb-install-stamp' -Force | Out-Null"

build-web: $(NM_STAMP) ## 构建前端 dist
	$(PS) "cd frontend; npm run build"

pack-assets: build-web ## 打包内嵌资源：扩展 bundle zip（依赖前端 dist）
	$(PS) "powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/pack-extension.ps1"

# 版本元数据同步：VERSION 是唯一权威，构建前写入各产物（wails.json 的 PE 版本资源 /
# package.json / 扩展 manifest 的四段版本）。必须排在 pack-assets 之前——manifest 的
# 版本要随 bundle 一起打包，否则装进安装包的是旧版本。
sync-version: ## 把 VERSION 同步到 wails.json / package.json / 扩展 manifest
	@$(GO) run ./cmd/syncversion -version $(VERSION) -root .

build: sync-version pack-assets ## 构建桌面端完整产物（VERSION 可覆盖；官方 wails 管线；自动停止运行中的桌面端并防构建期宿主复活锁文件）
	@powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/build-desktop.ps1 -App $(APP) -Version $(VERSION) -StopNames $(APP) -Gui -UseWails -WailsTool $(WAILS)

build-cli: sync-version pack-assets ## 构建 CLI 控制台版 exe（手动 go build 保持控制台子系统，wails 会强制 GUI；同时停止桌面端，避免桥服务双开）
	@powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/build-desktop.ps1 -App $(APP_CLI) -Version $(VERSION) -StopNames $(APP_CLI),$(APP) -GoTool $(GO)

ensure-iscc: ## 检测并自动安装 Inno Setup（固定 7.1.0，含中文语言包，已装则跳过）
	@$(PS) "powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ensure-iscc.ps1 -Ensure$(if $(ISCC), -IsccOverride '$(ISCC)')"

installer: build | ensure-iscc ## 生成 Inno Setup 安装程序（ISCC 环境不就绪则中止）
	@$(PS) "\$$iscc = & powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/ensure-iscc.ps1 -Resolve$(if $(ISCC), -IsccOverride '$(ISCC)'); if (-not \$$iscc) { Write-Host 'ISCC.exe (Inno Setup) not found. Run: make ensure-iscc'; exit 1 }; & \$$iscc /DMyAppVersion=$(VERSION) installer/setup.iss"

test-go: pack-assets ## 运行 Go 单元测试（禁用缓存，失败即停）
	$(GO) test -count=1 -timeout 300s -failfast ./...

test-web: $(NM_STAMP) ## 运行前端与扩展单元测试（vitest，tests/desktop + tests/extension）
	$(PS) "cd frontend; npx vitest run"

test-extension-e2e: $(E2E_STAMP) ## 扩展 E2E（Playwright 真实 Chromium；有头调试：先设 ABSORB_E2E_HEADED=1）
	$(PS) "cd e2e; npx playwright test -c extension/playwright.config.ts"

test: test-go test-web ## 运行全部测试（Go + 前端 + 扩展；E2E 慢，用 make test-extension-e2e 单独跑）

vet: pack-assets ## 运行 Go 静态分析（go vet ./...）
	$(GO) vet ./...

lint: pack-assets ## 运行 golangci-lint（需先安装；依赖 pack-assets 供 embed 类型检查）
	$(GOLANGCI) run ./...

govuln: ## 依赖漏洞扫描（需先安装 govulncheck）
	$(GOVULN) check ./...

eval: ## 运行 M0 评测（SET=评估集 ID 默认 understanding-basic；RUNS=每用例重复次数默认 3；结果落 design/eval/results/）
	$(GO) run ./cmd/absorb-eval -config config/config.yaml -data design/eval -out design/eval/results -set $(SET) -runs $(RUNS)

build-eval: ## 构建评测工具到 build/bin（开发期工具，不进安装包；临时重跑可用 eval 免构建）
	$(GO) build -o $(BIN_DIR)/absorb-eval.exe ./cmd/absorb-eval

fmt: ## 格式化 Go 源码（gofmt -s -w）
	$(GOFMT) -s -w .

release: pack-assets vet test installer ## 发布流水线：静态分析 + 全量测试 + 安装程序

run: build ## 构建并启动桌面端
	@$(PS) "Stop-Process -Name $(APP) -Force -ErrorAction SilentlyContinue; Start-Sleep 1; Start-Process -FilePath '$(EXE_PATH)'; Start-Sleep 6"

run-cli: build-cli ## 前台 CLI 服务模式，调试扩展链路（Ctrl+C 退出）
	./$(BIN_DIR)/$(APP_CLI).exe --cli

clean: ## 清理全部构建产物（exe、前端 dist、扩展 zip）
	@$(PS) "Stop-Process -Name $(APP) -Force -ErrorAction SilentlyContinue; Stop-Process -Name $(APP_CLI) -Force -ErrorAction SilentlyContinue; Start-Sleep 1; if (Test-Path '$(BIN_DIR)') { Remove-Item '$(BIN_DIR)' -Recurse -Force }; if (Test-Path 'frontend/dist') { Remove-Item 'frontend/dist' -Recurse -Force }; if (Test-Path 'internal/extension/extension_bundle.zip') { Remove-Item 'internal/extension/extension_bundle.zip' -Force }"
