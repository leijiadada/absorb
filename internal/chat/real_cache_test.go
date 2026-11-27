//go:build realmodel

// 真实模型缓存命中率探针（DeepSeek 服务端前缀缓存，64 token 块粒度）。
// 跑法：ABSORB_REAL_MODEL=1; go test -tags realmodel ./internal/chat -run TestRealCache -v
// 验证目标：
//  1. 技能概览稳定渲染 + 技能正文挂 tool result 后，system message 跨请求字节级一致，
//     同 scope 连续追问命中整个前缀（system + 历史），跨 scope 新问题至少命中整个 system。
//  2. 修复前：概览里逐请求的"Top recommended skill"行导致前缀在 system 头部（约 640 tok）分叉，
//     跨 scope 问题 cached≈640、同 scope 追问 cached≈|system|。
//
// 修复后预期：同 scope 追问 cached/prompt ≥ 60%；跨 scope 新问题 cached ≥ 2000（≈完整 system+概览）。
// TestRealL1BudgetDegrade：L1 预算超限降级探针（从最旧轮剔除，不再报错死锁）。
package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/store"
	"absorb-agent/internal/telemetry/tokentracker"
)

// TestRealL1BudgetDegrade L1 预算超限降级探针。页正文统一被 6000 rune 截断头收纳，
// 每个带页轮次估算 ≈4K token；预算压到 8000 只容 1 轮，Q3 起 L1 应剔除最旧轮。
// 旧实现（超限报错终止）在 Q2 就会失败且无用量流水（报错轮不成完整轮次，窗口死锁）；
// 降级后三问全部完成、Q3 prompt 保持单轮量级（未随轮数线性叠加）。
func TestRealL1BudgetDegrade(t *testing.T) {
	if os.Getenv("ABSORB_REAL_MODEL") != "1" {
		t.Skip("设 ABSORB_REAL_MODEL=1 后才打真实模型（消耗 token 配额）")
	}
	cfg := conf.LoadConfig("../../config/config.yaml")
	cfg.Memory.Enabled = false
	cfg.Skills.Root = filepath.Join("..", "..", "skills")
	cfg.WorkingSet.L1MaxRounds = 4
	cfg.WorkingSet.L1MaxTokens = 8000 // 小预算强制触发降级（正常页轮 ≈4K/轮）

	svc, cleanup := newCacheProbeStore(t, cfg)
	defer cleanup()

	ps, err := NewPromptStrategy("")
	if err != nil {
		t.Fatalf("构建提示词策略失败: %v", err)
	}
	runner, err := NewRunner(cfg, func() string { return "" }, ps, svc, NewMemoryGate(false))
	if err != nil {
		t.Fatalf("构建 runner 失败: %v", err)
	}

	var mu sync.Mutex
	var recs []tokentracker.Record
	tokentracker.SetSink(func(r tokentracker.Record) {
		mu.Lock()
		defer mu.Unlock()
		recs = append(recs, r)
	})
	defer tokentracker.SetSink(nil)

	ts := httptest.NewServer(AskHandler(runner, ps, svc, AppName, "absorb-user", NewRunRegistry(), func() conf.WorkingSetConfig { return cfg.WorkingSet }, nil))
	defer ts.Close()
	client := &http.Client{Timeout: 5 * time.Minute}

	pageA := syntheticPage("degrade-a", "图论基础")
	pageB := syntheticPage("degrade-b", "数值优化")
	pageC := syntheticPage("degrade-c", "符号计算")
	sessionID := "degrade-probe-" + time.Now().Format("01021504")

	// Q1 scope A 长页（L1 空）；Q2 scope B 长页（L1=[Q1轮]≈4K 预算内）；
	// Q3 scope C 长页（L1=[Q1,Q2]≈8K+ 超限 → 剔除 Q1 轮，prompt 不叠加）
	postProbeAsk(t, client, ts, fmt.Sprintf(`{"question":"这篇第一部分讲了什么？简要概括。","sessionId":%q,"context":{"url":"https://example.test/degrade-a","pageMd":%q,"contentStatus":"full"}}`, sessionID, pageA))
	postProbeAsk(t, client, ts, fmt.Sprintf(`{"question":"这篇第一部分讲了什么？简要概括。","sessionId":%q,"context":{"url":"https://example.test/degrade-b","pageMd":%q,"contentStatus":"full"}}`, sessionID, pageB))
	postProbeAsk(t, client, ts, fmt.Sprintf(`{"question":"这篇第一部分讲了什么？简要概括。","sessionId":%q,"context":{"url":"https://example.test/degrade-c","pageMd":%q,"contentStatus":"full"}}`, sessionID, pageC))

	mu.Lock()
	defer mu.Unlock()
	if len(recs) < 3 {
		t.Fatalf("期望 3 条用量流水（三问全部完成），实际 %d: %+v", len(recs), recs)
	}
	for i, r := range recs[:3] {
		t.Logf("Q%d prompt=%d completion=%d cached=%d", i+1, r.PromptTokens, r.CompletionTokens, r.CachedTokens)
	}
	// Q2 的 L1=[Q1轮]（预算内不裁剪），Q3 的 L1 应只保留 Q2 轮（剔除 Q1 轮），
	// 两问 prompt 同量级；未降级时 Q3 会叠加两轮（≈ Q2 + 4K+）
	q2, q3 := recs[1], recs[2]
	if q3.PromptTokens > q2.PromptTokens+2000 {
		t.Errorf("Q3 prompt=%d 超出 Q2 prompt=%d+2K：L1 未剔除最旧轮，降级未生效", q3.PromptTokens, q2.PromptTokens)
	}
}

// TestRealCacheHitRate 五问流水：同 scope 首问 → 同 scope 追问 → 连续三个跨 scope 新问题。
// 通过 tokentracker sink 捕获每次调用的 prompt/cached 实测值（与 StatusView 同口径）。
func TestRealCacheHitRate(t *testing.T) {
	if os.Getenv("ABSORB_REAL_MODEL") != "1" {
		t.Skip("设 ABSORB_REAL_MODEL=1 后才打真实模型（消耗 token 配额）")
	}
	cfg := conf.LoadConfig("../../config/config.yaml") // 解析失败时 LoadConfig 内部 panic
	// 探针独立于本地状态：记忆关闭（不碰真实 memory.db/profile.md），技能指向仓库真实技能目录
	cfg.Memory.Enabled = false
	cfg.Skills.Root = filepath.Join("..", "..", "skills")

	svc, cleanup := newCacheProbeStore(t, cfg)
	defer cleanup()

	ps, err := NewPromptStrategy("")
	if err != nil {
		t.Fatalf("构建提示词策略失败: %v", err)
	}
	runner, err := NewRunner(cfg, func() string { return "" }, ps, svc, NewMemoryGate(false))
	if err != nil {
		t.Fatalf("构建 runner 失败: %v", err)
	}

	// 捕获每次调用的用量流水；handler 对每条 SSE 流程 Track 一次
	var mu sync.Mutex
	var recs []tokentracker.Record
	tokentracker.SetSink(func(r tokentracker.Record) {
		mu.Lock()
		defer mu.Unlock()
		recs = append(recs, r)
	})
	defer tokentracker.SetSink(nil)

	ts := httptest.NewServer(AskHandler(runner, ps, svc, AppName, "absorb-user", NewRunRegistry(), func() conf.WorkingSetConfig { return cfg.WorkingSet }, nil))
	defer ts.Close()
	client := &http.Client{Timeout: 5 * time.Minute}

	pageA := syntheticPage("article-a", "流处理与概率方法")
	pageB := syntheticPage("article-b", "设计模式总览")
	sessionID := "cache-probe-" + time.Now().Format("01021504")

	// Q1 同 scope 首问：缓存为空，预期 cached≈0（本问将前缀写入服务端缓存）
	postProbeAsk(t, client, ts, fmt.Sprintf(`{"question":"这篇文章第一部分讲了什么？请简要概括。","sessionId":%q,"context":{"url":"https://example.test/article-a","pageMd":%q,"contentStatus":"full"}}`, sessionID, pageA))
	// Q2 同 scope 追问：不带 pageMd（沿用会话页 URL），前缀 = system+Q1 问+答，预期命中大部分
	postProbeAsk(t, client, ts, fmt.Sprintf(`{"question":"把刚才那段的核心结论再用一句话说清楚。","sessionId":%q}`, sessionID))
	// Q3 跨 scope 新问题：scope B 历史不可见，前缀只有 system+概览，预期命中完整 system（修复前仅 ~640）
	postProbeAsk(t, client, ts, fmt.Sprintf(`{"question":"这篇文章的整体结构是怎样的？","sessionId":%q,"context":{"url":"https://example.test/article-b","pageMd":%q,"contentStatus":"full"}}`, sessionID, pageB))
	// Q4/Q5 连续跨 scope：窗口 4 轮内 L1 头部不滑动（Q1 轮仍在），cached 应逐问累积不回退
	pageC := syntheticPage("article-c", "检索与排序")
	pageD := syntheticPage("article-d", "分布式一致性")
	postProbeAsk(t, client, ts, fmt.Sprintf(`{"question":"这篇里哪个概念最关键？","sessionId":%q,"context":{"url":"https://example.test/article-c","pageMd":%q,"contentStatus":"full"}}`, sessionID, pageC))
	postProbeAsk(t, client, ts, fmt.Sprintf(`{"question":"这篇文章的结论适用于什么场景？","sessionId":%q,"context":{"url":"https://example.test/article-d","pageMd":%q,"contentStatus":"full"}}`, sessionID, pageD))

	mu.Lock()
	defer mu.Unlock()
	if len(recs) < 5 {
		t.Fatalf("期望 5 条用量流水，实际 %d: %+v", len(recs), recs)
	}
	hit := func(r tokentracker.Record) float64 {
		if r.PromptTokens == 0 {
			return 0
		}
		return float64(r.CachedTokens) / float64(r.PromptTokens)
	}
	for i, r := range recs[:5] {
		t.Logf("Q%d prompt=%d completion=%d cached=%d hit=%.1f%%", i+1, r.PromptTokens, r.CompletionTokens, r.CachedTokens, hit(r)*100)
	}

	q2, q3, q4, q5 := recs[1], recs[2], recs[3], recs[4]
	if q2.CachedTokens == 0 || hit(q2) < 0.6 {
		t.Errorf("Q2 同 scope 追问命中率过低: cached=%d prompt=%d (%.1f%%)，期望 ≥60%%（system+历史应命中）", q2.CachedTokens, q2.PromptTokens, hit(q2)*100)
	}
	if q3.CachedTokens < 2000 {
		t.Errorf("Q3 跨 scope 新问题 cached=%d，期望 ≥2000（system+技能概览应整体命中；若 ≈640 说明概览仍在逐请求变化）", q3.CachedTokens)
	}
	// 窗口未滑动时跨 scope 命中只增不减：Q4 前缀含 Q3 全部已缓存内容，Q5 同理
	if q4.CachedTokens < q3.CachedTokens {
		t.Errorf("Q4 cached=%d < Q3 cached=%d：L1 窗口发生滑动（最旧轮被挤出）或前缀不稳定", q4.CachedTokens, q3.CachedTokens)
	}
	if q5.CachedTokens < q4.CachedTokens {
		t.Errorf("Q5 cached=%d < Q4 cached=%d：L1 窗口发生滑动或预算不足触发裁剪/报错", q5.CachedTokens, q4.CachedTokens)
	}
}

// newCacheProbeStore 独立临时会话库 + 注册 config.yaml 里的真实模型为默认配置。
// dynamicModel 从库中读取默认模型构建 openai 客户端，与生产同链路。
func newCacheProbeStore(t *testing.T, cfg conf.Config) (*store.SessionService, func()) {
	t.Helper()
	svc, err := store.NewSessionService(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("创建临时会话库失败: %v", err)
	}
	mc := &store.ModelConfig{
		ID:        "probe-model",
		Provider:  "deepseek",
		ModelID:   cfg.Model.Name,
		APIFormat: "openai_chat",
		BaseURL:   cfg.Model.BaseURL,
		APIKey:    cfg.Model.APIKey,
		Temperature: func() *float64 {
			v := cfg.Model.Temperature
			return &v
		}(),
		Thinking: 2, // 显式关思考（与 config.yaml thinking.enabled=false 一致），缓存口径稳定
	}
	ctx := context.Background()
	if err := svc.CreateModelConfig(ctx, mc); err != nil {
		t.Fatalf("注册探针模型失败: %v", err)
	}
	if err := svc.UpdateAppState(ctx, AppName, session.StateMap{"default_model_id": []byte(mc.ID)}); err != nil {
		t.Fatalf("设默认模型失败: %v", err)
	}
	return svc, func() { _ = svc.Close() }
}

// syntheticPage 生成确定性的长页正文（约 12K 字符英文技术文），足够把 Q1 prompt 推到数千 token。
// 主题词随 id/主题 变化，保证 Q1/Q3 页面内容互不相同（模拟真实跨文章提问）。
func syntheticPage(id, topic string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", topic)
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, "## Section %d (%s %s)\n\n", i+1, id, topic)
		for j := 0; j < 3; j++ {
			fmt.Fprintf(&b, "Paragraph %d-%d of %s discusses the core ideas around %s, including the trade-offs, the underlying assumptions, and the practical implications for real systems. The argument builds on earlier sections and prepares the ground for the next chapter, with concrete examples and counterexamples.\n\n", i+1, j+1, id, topic)
		}
	}
	return b.String()
}

// postProbeAsk 发送一次问答并等待 SSE 流程完整结束（AskHandler 同步返回）。
func postProbeAsk(t *testing.T, client *http.Client, ts *httptest.Server, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/ask", strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	// 流程结束才返回；这里只需读完，事件断言由用量流水承担
	buf := make([]byte, 4096)
	for {
		_, err := resp.Body.Read(buf)
		if err != nil {
			break
		}
	}
}
