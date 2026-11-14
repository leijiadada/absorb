package chat

// 全链路回归：scope 标识定长哈希，父子 URL 互不触发前缀聚合。
//
// 场景（真实链路 /api/ask → handler → SDK 请求体）：
//   1. 子页面 C（.../docs/03-features/04-chunking）先有一轮问答并被强制摘要
//      （Summaries[ScopeID(C)] 落库，boundary 覆盖 C 的轮次）；
//   2. 父页面 P（.../docs/03-features）有一轮问答；
//   3. 用户回到 P 提问。
//
// 修复前（裸 URL filterKey + Prefix 模式）：SDK 在 P 上精确匹配不到摘要，
// 按 "/" 前缀聚合 C 的摘要，cutoff 取 C 的 boundary 并按全会话事件下标一刀切，
// P 在 cutoff 前的原始事件被剔除；L1 去重又把 P 轮当"分支内重复"丢弃
// → 请求中 P 的历史完全消失。
//
// 修复后（ScopeID 定长哈希）：标识互不为 "/" 前缀，聚合永不触发、无 cutoff，
// P 的历史由 SDK 全量重放恰好 1 次；C 轮作为跨 scope 轮次由 L1 注入恰好 1 次；
// C 的摘要不会被注入到 P 的请求（聚合路径已不存在）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session"
	summarysdk "trpc.group/trpc-go/trpc-agent-go/session/summary"

	"absorb-agent/internal/store"
)

// fixedSummarizer 返回固定摘要文本的假摘要器：
// setup 的 NewRunner 会注入真摘要器（打向 SSE 假端点必失败），此处覆盖它，
// 让"子 scope 有摘要"这个前提不依赖 LLM。
type fixedSummarizer struct {
	summarysdk.SessionSummarizer
	text string
}

func (f fixedSummarizer) Summarize(context.Context, *session.Session) (string, error) {
	return f.text, nil
}

const (
	scopeIDParentURL = "https://scope-id.example.com/docs/03-features"
	scopeIDChildURL  = "https://scope-id.example.com/docs/03-features/04-chunking"
)

func TestAskHandler_ScopeIDKillsPrefixAggregation(t *testing.T) {
	h, cap2, svc := setup(t, "openai", true, false, defaultOKStream)
	ctx := context.Background()
	const sessionID = "s-scope-id"
	key := session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID}
	sess, err := svc.CreateSession(ctx, key, nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 1. 子 scope 一轮问答（哈希打标），并强制生成摘要（假摘要器，不依赖 LLM）
	svc.SetSummarizer(fixedSummarizer{text: "子页面文章的摘要内容"}, store.SummaryRunnerOptions{})
	wsSeedRound(t, svc, sess, "子页面的问题", "子页面的回答CHILD", scopeIDChildURL)
	if err := svc.CreateSessionSummary(ctx, sess, store.ScopeID(scopeIDChildURL), true); err != nil {
		t.Fatalf("CreateSessionSummary(child): %v", err)
	}
	// 确认摘要确已落库（前提校验）
	if _, ok := func() (string, bool) {
		sess.SummariesMu.RLock()
		defer sess.SummariesMu.RUnlock()
		s := sess.Summaries[store.ScopeID(scopeIDChildURL)]
		if s == nil {
			return "", false
		}
		return s.Summary, true
	}(); !ok {
		t.Fatal("子 scope 摘要未落库，测试前提不成立")
	}
	// 2. 父 scope 一轮问答
	wsSeedRound(t, svc, sess, "父页面的问题", "父页面的回答PARENT", scopeIDParentURL)

	// 3. 回到父页面提问
	out := wsAsk(t, h, sessionID, scopeIDParentURL, "这个章节除了刚才说的还讲了什么？")
	if strings.Contains(out, `"type":"error"`) {
		t.Fatalf("不应报错，SSE: %s", out)
	}
	reqBody := cap2.get()

	// 父页面历史必须完整在场（恰好 1 次：SDK scope 重放，无 cutoff 剔除）
	if n := strings.Count(reqBody, "父页面的回答PARENT"); n != 1 {
		t.Errorf("父页面历史应恰好 1 次，实际 %d；请求体：%s", n, reqBody)
	}
	if n := strings.Count(reqBody, "父页面的问题"); n != 1 {
		t.Errorf("父页面提问应恰好 1 次，实际 %d", n)
	}
	// 子页面摘要不得被聚合注入（前缀聚合路径已不存在）
	if strings.Contains(reqBody, "以上为压缩摘要") {
		t.Errorf("子页面摘要不应被聚合进父页面请求（聚合已不可能）")
	}
	// 子页面最近轮次作为跨 scope 连续性由 L1 注入恰好 1 次（工作集价值保持）
	if n := strings.Count(reqBody, "子页面的回答CHILD"); n != 1 {
		t.Errorf("子页面轮次应经 L1 跨 scope 注入恰好 1 次，实际 %d", n)
	}
}

// TestAskHandler_ScopeIDFormatInLogsAndMapping 校验 handler 双写日志（标识+网址）
// 与映射表登记：标识为 16 位十六进制，映射可还原出归一网址。
func TestAskHandler_ScopeIDFormatInLogsAndMapping(t *testing.T) {
	h, _, svc := setup(t, "openai", true, false, defaultOKStream)
	out := askWithPage(t, h, "https://Log.example.com/post/1?utm_source=x#frag", "")
	if strings.Contains(out, `"type":"error"`) {
		t.Fatalf("请求不应报错，SSE: %s", out)
	}
	m := strings.Index(out, `"sessionId":"`)
	if m < 0 {
		t.Fatalf("SSE 未含 sessionId: %s", out)
	}
	rest := out[m+len(`"sessionId":"`):]
	sessionID := rest[:strings.Index(rest, `"`)]

	wantID := store.ScopeID("https://log.example.com/post/1")
	if len(wantID) != store.ScopeIDLen {
		t.Fatalf("标识长度异常：%q", wantID)
	}
	urls, err := svc.ListScopeURLs(context.Background(), AppName, "absorb-user", sessionID)
	if err != nil {
		t.Fatalf("ListScopeURLs: %v", err)
	}
	if got := urls[wantID]; got != "https://log.example.com/post/1" {
		t.Fatalf("映射登记缺失或未归一：urls[%q]=%q（全部 %v）", wantID, got, urls)
	}
	// 事件按哈希打标：标识下可索引到本轮事件（落库异步，轮询等待）
	key := session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID}
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := svc.ListEventIndex(context.Background(), key, wantID)
		if err != nil {
			t.Fatalf("ListEventIndex: %v", err)
		}
		if len(entries) >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("标识 %q 下未索引到事件（打标可能仍用裸 URL）", wantID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAskHandler_SummaryLogCarriesURL 摘要日志必须带 url= 字段（标识人读还原）。
// 日志文本断言在冒烟层（absorb.log）完成；本用例验证"摘要落库 + 映射可还原"链路。
func TestAskHandler_SummaryLogCarriesURL(t *testing.T) {
	_, _, svc := setup(t, "openai", true, false, defaultOKStream)
	ctx := context.Background()
	const sessionID = "s-scope-id-log"
	key := session.Key{AppName: AppName, UserID: "absorb-user", SessionID: sessionID}
	sess, err := svc.CreateSession(ctx, key, nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	svc.SetSummarizer(fixedSummarizer{text: "摘要日志测试"}, store.SummaryRunnerOptions{})
	wsSeedRound(t, svc, sess, "摘要日志问题", "摘要日志回答", scopeIDChildURL)
	if err := svc.UpsertScopeURL(ctx, AppName, "absorb-user", sessionID,
		store.ScopeID(scopeIDChildURL), scopeIDChildURL); err != nil {
		t.Fatalf("UpsertScopeURL: %v", err)
	}
	if err := svc.CreateSessionSummary(ctx, sess, store.ScopeID(scopeIDChildURL), true); err != nil {
		t.Fatalf("CreateSessionSummary: %v", err)
	}
	// 摘要确已落库，且标识可还原为网址（日志 url= 字段的数据源）
	sess.SummariesMu.RLock()
	sum := sess.Summaries[store.ScopeID(scopeIDChildURL)]
	sess.SummariesMu.RUnlock()
	if sum == nil || !strings.Contains(sum.Summary, "摘要日志测试") {
		t.Fatalf("摘要未落库：%+v", sum)
	}
	got, err := svc.ScopeURL(ctx, AppName, "absorb-user", sessionID, store.ScopeID(scopeIDChildURL))
	if err != nil || got != scopeIDChildURL {
		t.Fatalf("ScopeURL=%q err=%v", got, err)
	}
}
