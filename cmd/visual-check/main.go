// visual-check 端到端 CLI 回归入口：visual-explanation 链路人工排查工具。
// 复用生产组装（conf → chat.NewRunner，见 agent.go），调真实 LLM 验证：
// ① 复杂机制题命中可视化白名单 → 回答组织在 :::absorb-visual 容器内；
// ② 简单术语题 → 不触发可视化；
// ③ 组件选型（SKILL.md 决策表）：时序题出 sequenceDiagram、对比题出 ≤4 数据列的 Markdown 表格、
// 数量对比题出 chart 组件（围栏/冒号两种写法都认）。
// 用法：go run ./cmd/visual-check（读 config/config.yaml，回答落盘系统临时目录，
// 退出码 0=全部符合预期）。
// 注意：同一份用例清单已有 integration 测试版（agent_integration_test.go，
// ABSORB_E2E=1 go test -tags=integration ./cmd/visual-check/ -v），失败自动归到命名子测试；
// 本 CLI 用于人工跑查 + 落盘全文。
// 改动 glm-system.md / SKILL.md 后建议重跑其一。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/log"
)

func main() {
	// 配置里的相对路径（skills.root、memory.db_path 等）以仓库根为基准，
	// 先切到仓库根再加载，避免从其他目录启动时静默丢技能/记忆。
	cfgPath, err := filepath.Abs(defaultConfigPath)
	ck(err)
	if _, err := chdirRepoRoot(cfgPath); err != nil {
		ck(err)
	}
	cfg := conf.LoadConfig(cfgPath)
	if err := log.InitLogging(cfg.Log); err != nil {
		fatal("init logging: %v", err)
	}

	exitCode := 0
	for _, c := range regressionCases() {
		env, err := newCheckEnv(cfgPath, false)
		ck(err)

		answer, err := env.ask(context.Background(), c.question, "cli-"+c.id)
		ck(err)
		env.closer()

		out, err := os.CreateTemp("", "absorb-visual-"+c.id+"-*.md")
		ck(err)
		_, err = out.WriteString(answer)
		ck(err)
		ck(out.Close())

		ok, asserts := c.evaluate(answer)
		fmt.Printf("[%s] %v -> %s\n回数=%d 输出=%s\n\n", c.id, asserts, pass(ok), len(answer), out.Name())
		if !ok {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

func pass(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func ck(err error) {
	if err != nil {
		fatal("%v", err)
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "visual-check: "+f+"\n", a...)
	os.Exit(2)
}
