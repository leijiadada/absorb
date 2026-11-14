// visual-check 共用组装与断言逻辑：生产链路（conf → chat.NewRunner，与
// cmd/absorb-eval 同款）封装成 newCheckEnv，供 main.go（CLI 入口）与
// check_test.go（integration 端到端回归）共用。CLI 与测试各自独立组装环境。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"absorb-agent/internal/chat"
	"absorb-agent/internal/conf"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
)

// initCheckLog 日志初始化（仅 CLI 路径使用；测试走 t.Logf，避免重复装配）。
func initCheckLog(cfg conf.Config) error {
	return log.InitLogging(cfg.Log)
}

// defaultConfigPath CLI 视角的配置路径（相对当前工作目录仓库根）。
const defaultConfigPath = "config/config.yaml"

// checkEnv 一次隔离的回归环境：临时 sqlite + 独立 runner，用完必须 closer()。
type checkEnv struct {
	// ask 发起一次真实 LLM 问答，返回最终回答全文（流式增量兜底）
	ask func(ctx context.Context, question, sessionID string) (string, error)
	// closer 释放 runner/会话服务并删除临时目录，幂等不保证，只调一次
	closer func()
}

// newCheckEnv 组装隔离环境。initLog=false 时不初始化日志（测试并行下避免重复装配）。
func newCheckEnv(configPath string, initLog bool) (*checkEnv, error) {
	cfg := conf.LoadConfig(configPath)
	if initLog {
		if err := initCheckLog(cfg); err != nil {
			return nil, err
		}
	}

	tmpDir, err := os.MkdirTemp("", "absorb-visual-check-*")
	if err != nil {
		return nil, err
	}
	dbPath, err := filepath.Abs(filepath.Join(tmpDir, "sessions.db"))
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}
	cfg.Store.Backend = "sqlite"
	cfg.Store.SQLite.Path = dbPath

	sessSvc, err := store.New(cfg.Store.Backend, dbPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}

	ctx := context.Background()
	migrated, err := sessSvc.MigrateLegacyModelConfig(ctx, chat.AppName,
		cfg.Model.Name, cfg.Model.APIKey, cfg.Model.BaseURL,
		cfg.Model.Temperature, cfg.Model.MaxTokens, cfg.Model.Thinking.Enabled)
	if err != nil {
		sessSvc.Close()
		os.RemoveAll(tmpDir)
		return nil, err
	}
	if !migrated {
		sessSvc.Close()
		os.RemoveAll(tmpDir)
		return nil, errors.New("没有可用默认模型（config.yaml 未配置完整模型信息）")
	}

	gate := chat.NewMemoryGate(false)
	strategy, err := chat.NewPromptStrategy(cfg.Model.Variant, chat.WithProfileGate(gate))
	if err != nil {
		sessSvc.Close()
		os.RemoveAll(tmpDir)
		return nil, err
	}
	r, err := chat.NewRunner(cfg, func() string { return cfg.Proxy.URL }, strategy, sessSvc, gate)
	if err != nil {
		sessSvc.Close()
		os.RemoveAll(tmpDir)
		return nil, err
	}

	ask := func(actx context.Context, question, sessionID string) (string, error) {
		prep, err := chat.PrepareInvocation(actx, chat.AppName, "cli-user", chat.InvocationInput{
			Question: question, SessionID: sessionID,
		}, chat.InvocationDeps{
			SessionSvc: sessSvc,
			PS:         strategy,
			WSFn:       func() conf.WorkingSetConfig { return cfg.WorkingSet },
		})
		if err != nil {
			return "", err
		}

		var sb strings.Builder
		var finalFull string
		ch, err := r.Run(prep.RunCtx, "cli-user", prep.SessionID, prep.Message, prep.RunOpts...)
		if err != nil {
			return "", err
		}
		for evt := range ch {
			if evt.Response == nil || len(evt.Response.Choices) == 0 {
				continue
			}
			if choice := evt.Response.Choices[0]; choice.Delta.Content != "" {
				sb.WriteString(choice.Delta.Content)
			}
			if evt.IsFinalResponse() {
				if s := evt.Response.Choices[0].Message.Content; s != "" {
					finalFull = s
				}
			}
		}
		if finalFull == "" {
			return sb.String(), nil
		}
		return finalFull, nil
	}

	return &checkEnv{
		ask: ask,
		closer: func() {
			r.Close()
			sessSvc.Close()
			os.RemoveAll(tmpDir)
		},
	}, nil
}

// chdirRepoRoot 切换进程工作目录到仓库根（含 go.mod 的最近祖先），返回该路径。
// 配置里的相对路径（skills.root、memory.db_path、store.sqlite.path）都以仓库根为基准；
// go test 的包目录会让它们全部解析失败，端到端环境因此失真（技能被跳过、记忆库打不开）。
func chdirRepoRoot(configPath string) (string, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", err
	}
	fallback := filepath.Dir(abs)
	root := fallback
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root { // 到盘符根仍未找到 go.mod：退回配置目录
			root = fallback
			break
		}
		root = parent
	}
	if err := os.Chdir(root); err != nil {
		return "", err
	}
	return root, nil
}

// visualCase 一条端到端回归用例。
type visualCase struct {
	id, question string
	wantVisual   bool
	// mustContain：回答中必须全部出现的子串（选型断言，如时序题必须出 sequenceDiagram）
	mustContain []string
	// anyOf：回答中至少出现一个即可（同一组件有多种合法写法时用，如 chart 的围栏/冒号）
	anyOf []string
	// maxTableDataCols：Markdown 表格数据列数上限（0=不检查），防止对比表撑破侧栏
	maxTableDataCols int
}

// evaluate 对回答执行全部断言，返回是否通过与断言明细（人工排查/测试失败信息共用）。
func (c visualCase) evaluate(answer string) (bool, []string) {
	has := strings.Contains(answer, ":::absorb-visual")
	ok := has == c.wantVisual
	asserts := []string{fmt.Sprintf("visual=%v", c.wantVisual)}
	// 内容断言：不论是否可视化都必须命中，避免空回答/跑题也通过（simple 只查容器时尤其危险）
	for _, sub := range c.mustContain {
		if !strings.Contains(answer, sub) {
			ok = false
			asserts = append(asserts, fmt.Sprintf("missing %q", sub))
		}
	}
	// 多写法断言：任一命中即可（chart 既可能写 ```chart bar 也可能写 :::chart bar）
	if len(c.anyOf) > 0 {
		found := -1
		for i, sub := range c.anyOf {
			if strings.Contains(answer, sub) {
				found = i
				break
			}
		}
		if found < 0 {
			ok = false
			asserts = append(asserts, fmt.Sprintf("missing any of %q", c.anyOf))
		} else {
			asserts = append(asserts, fmt.Sprintf("found %q", c.anyOf[found]))
		}
	}
	// 对比题：数据列数 ≤ 上限（表格行按 | 切分计数，忽略首尾空段）
	if c.maxTableDataCols > 0 {
		maxCols := 0
		for _, line := range strings.Split(answer, "\n") {
			t := strings.TrimSpace(line)
			if !strings.HasPrefix(t, "|") || !strings.HasSuffix(t, "|") {
				continue
			}
			cols := strings.Count(strings.Trim(t, "|"), "|") + 1
			if cols > maxCols {
				maxCols = cols
			}
		}
		if maxCols > c.maxTableDataCols {
			ok = false
			asserts = append(asserts, fmt.Sprintf("table cols %d > %d", maxCols, c.maxTableDataCols))
		} else {
			asserts = append(asserts, fmt.Sprintf("table cols %d ≤ %d", maxCols, c.maxTableDataCols))
		}
	}
	return ok, asserts
}

// regressionCases 全量回归用例（CLI 与 integration 测试共用同一份清单）。
func regressionCases() []visualCase {
	return []visualCase{
		{id: "complex", question: "TCP 三次握手为什么需要三次？请图示化讲清楚整个过程。",
			wantVisual: true, mustContain: []string{"握手"}},
		{id: "simple", question: "Redis 是什么？", mustContain: []string{"Redis"}},
		{id: "sequence", question: "TCP 挥手为什么比握手多一次？请图示化讲清楚双方报文交互。",
			wantVisual: true, mustContain: []string{"sequenceDiagram"}},
		{id: "compare", question: "RAG、微调、长上下文三种方案怎么选？请对比并图示化。",
			wantVisual: true, mustContain: []string{"|"}, maxTableDataCols: 4},
		// 数量对比：走 chart 组件（新组件回归）。两种写法都合法，任一命中即可。
		{id: "chart", question: "Go、Rust、C 三门语言编译同一个 Hello World 的耗时大概各是多少？请用图表对比这三者的数值。",
			wantVisual: true, anyOf: []string{"```chart", ":::chart"}},
	}
}
