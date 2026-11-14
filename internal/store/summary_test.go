package store

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
	summarysdk "trpc.group/trpc-go/trpc-agent-go/session/summary"
)

// recordingSummarizer 记录每次摘要实际收到的增量事件，并模拟 SDK 写入摘要边界状态。
// texts 非空时按调用次序返回脚本化摘要文本（滚动累积断言用）。
type recordingSummarizer struct {
	summarysdk.SessionSummarizer

	mu    sync.Mutex
	calls [][]event.Event
	texts []string
}

func (r *recordingSummarizer) Summarize(_ context.Context, sess *session.Session) (string, error) {
	events := append([]event.Event(nil), sess.Events...)
	r.mu.Lock()
	idx := len(r.calls)
	r.calls = append(r.calls, events)
	text := "测试摘要"
	if len(r.texts) > 0 {
		text = r.texts[min(idx, len(r.texts)-1)]
	}
	r.mu.Unlock()

	if len(events) > 0 {
		last := events[len(events)-1]
		sess.SetState(session.SummaryLastIncludedTimestampStateKey,
			[]byte(last.Timestamp.UTC().Format(time.RFC3339Nano)))
		sess.SetState(session.SummaryLastIncludedEventIDStateKey, []byte(last.ID))
	}
	return text, nil
}

// panickingSummarizer 摘要调用直接 panic（验证 worker 的 recover 兜底）。
type panickingSummarizer struct {
	summarysdk.SessionSummarizer
}

func (panickingSummarizer) Summarize(context.Context, *session.Session) (string, error) {
	panic("boom: summary model call")
}

func (r *recordingSummarizer) snapshotCalls() [][]event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]event.Event, len(r.calls))
	for i := range r.calls {
		out[i] = append([]event.Event(nil), r.calls[i]...)
	}
	return out
}

func appendSummaryEvent(
	t *testing.T,
	svc *SessionService,
	sess *session.Session,
	id, role, content, filterKey string,
	ts time.Time,
) {
	t.Helper()
	evt := mkScopedEvent(role, content, filterKey, ts)
	evt.ID = id
	// Version 必须置当前版本：evt.Filter 仅在版本匹配时读 FilterKey，
	// 否则回退 Branch（空 → 恒真），级联判定的 allEventsInScope 会失真。
	evt.Version = event.CurrentVersion
	if err := svc.AppendEvent(context.Background(), sess, evt); err != nil {
		t.Fatalf("AppendEvent(%s): %v", id, err)
	}
}

// TestSummaryScopeBoundaryIsolationRegression 回归：增量边界按 scope 隔离（读
// Summaries[filterKey].Boundary，不读会话级共享 state 键）。A 先摘要到较新时间后，
// B 的较旧未摘要事件仍会进入 B 自己的摘要（修复前被 A 的全局游标永久跳过）。
func TestSummaryScopeBoundaryIsolationRegression(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	recorder := &recordingSummarizer{}
	svc.SetSummarizer(recorder, SummaryRunnerOptions{QueueSize: 1})

	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "summary-scope-repro"}
	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: sess=%v err=%v", sess, err)
	}

	base := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	const scopeA = "https://a.example.com"
	const scopeB = "https://b.example.com"
	appendSummaryEvent(t, svc, sess, "b1", "user", "B 的旧问题", scopeB, base)
	appendSummaryEvent(t, svc, sess, "b2", "assistant", "B 的旧回答", scopeB, base.Add(time.Minute))
	appendSummaryEvent(t, svc, sess, "a1", "user", "A 的新问题", scopeA, base.Add(2*time.Minute))
	appendSummaryEvent(t, svc, sess, "a2", "assistant", "A 的新回答", scopeA, base.Add(3*time.Minute))

	if err := svc.CreateSessionSummary(ctx, sess, scopeA, true); err != nil {
		t.Fatalf("CreateSessionSummary(A): %v", err)
	}
	if err := svc.CreateSessionSummary(ctx, sess, scopeB, true); err != nil {
		t.Fatalf("CreateSessionSummary(B): %v", err)
	}

	// B 摘要必须发生，且收到的是 B 自己的全部事件（含早于 A 边界的 b1/b2）
	calls := recorder.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("scope 边界未隔离：summarizer calls=%d, want 2（B 摘要被 A 游标吞掉）", len(calls))
	}
	if got := eventIDs(calls[1]); len(got) != 2 || got[0] != "b1" || got[1] != "b2" {
		t.Fatalf("B 摘要事件不完整：got %v, want [b1 b2]", got)
	}
	sess.SummariesMu.RLock()
	_, hasB := sess.Summaries[scopeB]
	boundaryA := sess.Summaries[scopeA].Boundary
	sess.SummariesMu.RUnlock()
	if !hasB {
		t.Fatal("B 摘要未持久化到 Summaries[scopeB]")
	}
	if boundaryA == nil || boundaryA.LastEventID != "a2" {
		t.Fatalf("A 边界事件错误：%+v", boundaryA)
	}
	t.Log("已回归：per-scope 边界隔离，B 的较旧未摘要事件进入 B 自己的摘要")
}

// TestSummaryBoundaryEventNotRepeatedRegression 回归：增量判定以 Boundary.LastEventID
// 为准（按事件序严格取其后），时间等于 cutoff 的上一轮末事件不再重复进入摘要。
func TestSummaryBoundaryEventNotRepeatedRegression(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	recorder := &recordingSummarizer{}
	svc.SetSummarizer(recorder, SummaryRunnerOptions{QueueSize: 1})

	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "summary-boundary-repro"}
	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: sess=%v err=%v", sess, err)
	}

	base := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	const scope = "https://a.example.com"
	appendSummaryEvent(t, svc, sess, "e1", "user", "第一问", scope, base)
	appendSummaryEvent(t, svc, sess, "e2", "assistant", "第一答", scope, base.Add(time.Minute))
	if err := svc.CreateSessionSummary(ctx, sess, scope, true); err != nil {
		t.Fatalf("CreateSessionSummary(first): %v", err)
	}
	sess.SummariesMu.RLock()
	boundary := sess.Summaries[scope].Boundary
	sess.SummariesMu.RUnlock()
	if boundary == nil || boundary.LastEventID != "e2" {
		t.Fatalf("第一轮边界事件未写入 Boundary：%+v", boundary)
	}

	appendSummaryEvent(t, svc, sess, "e3", "user", "第二问", scope, base.Add(2*time.Minute))
	if err := svc.CreateSessionSummary(ctx, sess, scope, true); err != nil {
		t.Fatalf("CreateSessionSummary(second): %v", err)
	}

	calls := recorder.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("summarizer calls=%d, want 2", len(calls))
	}
	second := calls[1]
	// 滚动累积后输入头部是旧摘要合成事件（Author=system、无 ID），剥离后再断言增量
	if len(second) > 0 && second[0].Author == "system" {
		second = second[1:]
	}
	if len(second) != 1 || second[0].ID != "e3" {
		t.Fatalf("边界事件未被排除：第二轮事件 IDs=%v, want [e3]", eventIDs(second))
	}
	t.Log("已回归：Boundary.LastEventID 参与过滤，边界事件 e2 不再重复进入增量")
}

func eventIDs(events []event.Event) []string {
	ids := make([]string, len(events))
	for i := range events {
		ids[i] = events[i].ID
	}
	return ids
}

// TestRecentPageURLJSONEscapingRegression 回归：json.Marshal 会把 "<" 转义为 \u003c，
// RecentPageURL 须先解码事件再从消息文本提取，对原始 JSON 的字面量搜索才能命中。
func TestRecentPageURLJSONEscapingRegression(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recent-url-repro"}
	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: sess=%v err=%v", sess, err)
	}

	const want = "https://example.com/article?a=1&b=2"
	content := "<page_url>" + want + "</page_url>\n\n问题：测试"
	if err := svc.AppendEvent(ctx, sess, mkEvent("user", content)); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	got, err := svc.RecentPageURL(ctx, key.AppName, key.UserID, key.SessionID)
	if err != nil {
		t.Fatalf("RecentPageURL: %v", err)
	}
	if got != want {
		t.Fatalf("RecentPageURL=%q, want %q", got, want)
	}
	t.Log("已回归：JSON 转义后仍能解码提取 page_url")
}

// TestReferenceMaterialScopeIsolationRegression 回归：追问重申按 scope 隔离。
// 会话先在文章 A 划词产生参考材料，切到文章 B 追问（无新划词）不得取到 A 的
// 参考材料（宁缺勿错返回空）；回到 A 追问重申 A 最近一次参考材料；
// 无标事件（FilterKey=""）按 scope 语义纳入所有 scope，可被任一 scope 重申。
func TestReferenceMaterialScopeIsolationRegression(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "refmat-scope-repro"}
	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: sess=%v err=%v", sess, err)
	}

	const scopeA = "https://a.example.com/article"
	const scopeB = "https://b.example.com/article"
	base := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	matA1 := "<reference_material>第一段参考材料</reference_material>\n\n问题：A 是什么"
	matA2 := "<reference_material>第二段参考材料</reference_material>\n\n问题：A 还有"
	matU := "<reference_material>无标参考材料</reference_material>\n\n问题：通用"
	appendMat := func(filterKey, content string, ts time.Time) {
		t.Helper()
		if err := svc.AppendEvent(ctx, sess, mkScopedEvent("user", content, filterKey, ts)); err != nil {
			t.Fatalf("AppendEvent(%q): %v", filterKey, err)
		}
	}

	// 文章 A 划词 → 无标事件 → 文章 A 再划词（时间序）：
	// A scope 取最近一次带标材料（matA2），B scope 只命中无标材料（隔离 + 纳入）
	appendMat(scopeA, matA1, base)
	appendMat("", matU, base.Add(time.Minute))
	appendMat(scopeA, matA2, base.Add(2*time.Minute))

	// 切到文章 B 追问：B scope 无带标参考材料，但无标事件纳入 → 重申无标材料
	gotB, err := svc.RecentReferenceMaterial(ctx, key.AppName, key.UserID, key.SessionID, scopeB)
	if err != nil {
		t.Fatalf("RecentReferenceMaterial(B): %v", err)
	}
	if want := "无标参考材料"; gotB != want {
		t.Fatalf("B scope 重申=%q, want %q（无标事件应纳入）", gotB, want)
	}

	// 回到文章 A 追问：重申 A scope 内最近一次（第二段）
	gotA, err := svc.RecentReferenceMaterial(ctx, key.AppName, key.UserID, key.SessionID, scopeA)
	if err != nil {
		t.Fatalf("RecentReferenceMaterial(A): %v", err)
	}
	if want := "第二段参考材料"; gotA != want {
		t.Fatalf("A scope 重申=%q, want %q", gotA, want)
	}

	// 纯隔离验证：会话仅 A 有带标参考材料，C scope 追问应为空（宁缺勿错）
	key2 := session.Key{AppName: "absorb", UserID: "u1", SessionID: "refmat-scope-only-a"}
	if _, err := svc.CreateSession(ctx, key2, nil); err != nil {
		t.Fatalf("CreateSession(2): %v", err)
	}
	sess2, err := svc.GetSession(ctx, key2)
	if err != nil || sess2 == nil {
		t.Fatalf("GetSession(2): sess=%v err=%v", sess2, err)
	}
	if err := svc.AppendEvent(ctx, sess2, mkScopedEvent("user", matA1, scopeA, base)); err != nil {
		t.Fatalf("AppendEvent(only A): %v", err)
	}
	gotC, err := svc.RecentReferenceMaterial(ctx, key2.AppName, key2.UserID, key2.SessionID, "https://c.example.com/article")
	if err != nil {
		t.Fatalf("RecentReferenceMaterial(C): %v", err)
	}
	if gotC != "" {
		t.Fatalf("C scope 重申串到 A 的参考材料：%q", gotC)
	}
}

// TestReproEstimateTokensIgnoresStructuredPayloads 验证普通 Content 会被统计，同时复现
// ToolCalls.Arguments 和 ContentParts 文本未进入 token 阈值估算。
func TestReproEstimateTokensIgnoresStructuredPayloads(t *testing.T) {
	ctx := context.Background()
	normal := []event.Event{
		*mkScopedEvent("user", strings.Repeat("用户文本", 200), "", time.Now()),
		*mkScopedEvent("tool", strings.Repeat("工具结果", 200), "", time.Now()),
	}
	if got := estimateTokens(ctx, normal); got <= 0 {
		t.Fatalf("普通 user/tool Message.Content 应被统计，got %d", got)
	}

	args := []byte(`{"url":"` + strings.Repeat("x", 6000) + `"}`)
	partText := strings.Repeat("多模态文本", 1200)
	structured := []event.Event{
		{
			Response: &model.Response{Choices: []model.Choice{{Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-1",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "fetch_page",
						Arguments: args,
					},
				}},
			}}}},
		},
		{
			Response: &model.Response{Choices: []model.Choice{{Message: model.Message{
				Role: model.RoleUser,
				ContentParts: []model.ContentPart{{
					Type: model.ContentType("text"),
					Text: &partText,
				}},
			}}}},
		},
	}
	got := estimateTokens(ctx, structured)
	if got > 2 {
		t.Fatalf("复现条件变化：结构化载荷 token=%d；当前实现只读空 Message.Content，预期仅换行产生的极小值", got)
	}
	runner := &summaryRunner{tokenThreshold: 100}
	if runner.thresholdFired(ctx, structured) {
		t.Fatal("复现条件变化：仅含 ToolCalls/ContentParts 的大载荷触发了 token 阈值")
	}
	t.Logf("已复现：大体积 ToolCalls.Arguments 与 ContentParts 仅估算为 %d token，未触发阈值", got)
}

// TestSummaryWorkerCloseRaceRegression 在子进程中运行真实 worker，并制造生产代码允许的顺序：
// worker 所属 runner 仍在处理 job，但 SessionService.sum 已被 Close 清空。
// worker 必须使用自己的 runner 完成在途任务，不能重新读取 nil 后崩溃。
func TestSummaryWorkerCloseRaceRegression(t *testing.T) {
	if os.Getenv("ABSORB_REPRO_SUMMARY_CLOSE_RACE") == "1" {
		runSummaryWorkerCloseRaceChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSummaryWorkerCloseRaceRegression$", "-test.v")
	cmd.Env = append(os.Environ(), "ABSORB_REPRO_SUMMARY_CLOSE_RACE=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("关闭时序下 worker 子进程异常退出：\n%s", output)
	}
	if strings.Contains(string(output), "panic") {
		t.Fatalf("关闭时序下 worker 仍发生 panic：\n%s", output)
	}
	t.Log("已验证：Close 清空 SessionService.sum 后，在途 worker 仍使用所属 runner 安全完成")
}

func runSummaryWorkerCloseRaceChild(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	recorder := &recordingSummarizer{}
	svc.SetSummarizer(recorder, SummaryRunnerOptions{QueueSize: 1})
	runner := svc.sumRunner()
	if runner == nil {
		t.Fatal("summary runner 未启动")
	}

	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "summary-close-race"}
	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: sess=%v err=%v", sess, err)
	}
	appendSummaryEvent(t, svc, sess, "close-race-event", "user", "触发关闭竞态",
		"https://a.example.com", time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))

	// 放大 stopSummarizer 中 sum=nil 与 runner.close() 之间允许发生的调度窗口。
	// 此时旧 worker 仍然拥有 runner，必须用该 runner 处理已经取得的任务。
	svc.sumMu.Lock()
	svc.sum = nil
	svc.sumMu.Unlock()
	runner.jobs <- summaryJob{sess: sess, filterKey: "https://a.example.com", force: true}

	deadline := time.Now().Add(2 * time.Second)
	for len(runner.jobs) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(runner.jobs) != 0 {
		t.Fatal("worker 未及时取走回归测试 job")
	}
	runner.close()

	calls := recorder.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("在途摘要任务未完成：summarizer calls=%d", len(calls))
	}
	sess.SummariesMu.RLock()
	_, summarized := sess.Summaries["https://a.example.com"]
	sess.SummariesMu.RUnlock()
	if !summarized {
		t.Fatal("在途摘要任务未写回摘要")
	}
}
