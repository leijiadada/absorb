package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

// newRecallService 用临时目录 db 建 *SessionService（召回方法未进 session.Service
// 接口，测试需具体类型访问）。
func newRecallService(t *testing.T) *SessionService {
	t.Helper()
	svc, err := NewSessionService(filepath.Join(t.TempDir(), "recall.db"))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	return svc
}

// mkScopedEvent 构造一条带 scope 标记与时间戳的事件（召回用例的数据基座）。
func mkScopedEvent(role, content, filterKey string, ts time.Time) *event.Event {
	finish := "stop"
	return &event.Event{
		FilterKey: filterKey,
		Timestamp: ts,
		Response: &model.Response{
			ID:     "resp",
			Object: "chat.completion",
			Model:  "test",
			Choices: []model.Choice{{
				Index: 0,
				Message: model.Message{
					Role:    model.Role(role),
					Content: content,
				},
				FinishReason: &finish,
			}},
		},
	}
}

// appendScopedEvents 依次落库事件并返回（时间戳逐条递增 1 分钟，保证 covered 判定可控）。
func appendScopedEvents(t *testing.T, svc session.Service, sess *session.Session, base time.Time, evts ...[3]string) {
	t.Helper()
	for i, e := range evts {
		evt := mkScopedEvent(e[0], e[1], e[2], base.Add(time.Duration(i)*time.Minute))
		if err := svc.AppendEvent(context.Background(), sess, evt); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
}

// TestRecallScopeIsolation scope 过滤语义与摘要 scopedEvents 一致：
// filterKey 非空 → 该标事件 + 无标事件；filterKey 空 → 全会话。
func TestRecallScopeIsolation(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-scope"}
	base := time.Now()

	svc.CreateSession(ctx, key, nil)
	sess, _ := svc.GetSession(ctx, key)
	appendScopedEvents(t, svc, sess, base,
		[3]string{"user", "urlA 的问题", "https://a.example.com"},
		[3]string{"user", "urlB 的问题", "https://b.example.com"},
		[3]string{"assistant", "无标回答", ""},
	)

	getSeqs := func(filterKey string) []int64 {
		entries, err := svc.ListEventIndex(ctx, key, filterKey)
		if err != nil {
			t.Fatalf("ListEventIndex(%q): %v", filterKey, err)
		}
		var seqs []int64
		for _, e := range entries {
			seqs = append(seqs, e.Seq)
		}
		return seqs
	}
	// AppendEvent seq 从 1 自增
	if got := getSeqs("https://a.example.com"); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("scope A 应含 seq 1,3，got %v", got)
	}
	if got := getSeqs("https://b.example.com"); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("scope B 应含 seq 2,3，got %v", got)
	}
	if got := getSeqs(""); len(got) != 3 {
		t.Fatalf("空 scope 应查全会话（3 条），got %v", got)
	}
	// 不存在的 scope：只含无标事件
	if got := getSeqs("https://c.example.com"); len(got) != 1 || got[0] != 3 {
		t.Fatalf("陌生 scope 应仅含无标事件 seq 3，got %v", got)
	}
}

// TestFetchEventSliceBounds seq 闭区间边界：闭区间含端点、from>to 与越界返回空。
func TestFetchEventSliceBounds(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-bounds"}
	base := time.Now()

	svc.CreateSession(ctx, key, nil)
	sess, _ := svc.GetSession(ctx, key)
	appendScopedEvents(t, svc, sess, base,
		[3]string{"user", "q1", "https://a.example.com"},
		[3]string{"user", "q2", "https://a.example.com"},
		[3]string{"user", "q3", "https://a.example.com"},
		[3]string{"user", "q4", "https://a.example.com"},
		[3]string{"user", "q5", "https://a.example.com"},
	)

	getSeqs := func(from, to int64) []int64 {
		events, err := svc.FetchEventSlice(ctx, key, "https://a.example.com", from, to)
		if err != nil {
			t.Fatalf("FetchEventSlice(%d,%d): %v", from, to, err)
		}
		var seqs []int64
		for _, e := range events {
			seqs = append(seqs, e.Seq)
		}
		return seqs
	}
	if got := getSeqs(2, 4); len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Fatalf("[2,4] 闭区间应含 seq 2,3,4，got %v", got)
	}
	if got := getSeqs(4, 2); len(got) != 0 {
		t.Fatalf("from>to 应返回空，got %v", got)
	}
	if got := getSeqs(10, 20); len(got) != 0 {
		t.Fatalf("越界区间应返回空，got %v", got)
	}
	if got := getSeqs(5, 100); len(got) != 1 || got[0] != 5 {
		t.Fatalf("[5,100] 应含 seq 5，got %v", got)
	}
}

// TestSearchEventsScoring 关键词打分：命中词数降序、同分 seq 降序、limit 截断、
// 大小写不敏感。
func TestSearchEventsScoring(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-search"}
	base := time.Now()

	svc.CreateSession(ctx, key, nil)
	sess, _ := svc.GetSession(ctx, key)
	appendScopedEvents(t, svc, sess, base,
		[3]string{"user", "Transformer 的 attention 机制详解", "https://a.example.com"},   // seq1：命中 2 词
		[3]string{"assistant", "Attention is all you need", "https://a.example.com"}, // seq2：命中 1 词
		[3]string{"user", "完全不相关的其他内容", "https://a.example.com"},                     // seq3：零命中
		[3]string{"assistant", "再谈 attention 的变体", "https://a.example.com"},          // seq4：命中 1 词（同分，seq 更大排前）
	)

	searchSeqs := func(keywords []string, limit int) []int64 {
		events, err := svc.SearchEvents(ctx, key, "https://a.example.com", keywords, limit)
		if err != nil {
			t.Fatalf("SearchEvents: %v", err)
		}
		var seqs []int64
		for _, e := range events {
			seqs = append(seqs, e.Seq)
		}
		return seqs
	}
	// 双词：seq1 得分 2 第一；seq2/seq4 同分 1，seq4（新）在前
	if got := searchSeqs([]string{"attention", "Transformer"}, 0); len(got) != 3 || got[0] != 1 || got[1] != 4 || got[2] != 2 {
		t.Fatalf("打分排序不符，got %v", got)
	}
	// limit 截断
	if got := searchSeqs([]string{"attention", "Transformer"}, 1); len(got) != 1 || got[0] != 1 {
		t.Fatalf("limit=1 应只返回 seq1，got %v", got)
	}
	// 大小写不敏感
	if got := searchSeqs([]string{"TRANSFORMER"}, 0); len(got) != 1 || got[0] != 1 {
		t.Fatalf("大小写不敏感匹配失败，got %v", got)
	}
	// 全部无效关键词 → 空
	if got := searchSeqs([]string{"", "   "}, 0); len(got) != 0 {
		t.Fatalf("无效关键词应返回空，got %v", got)
	}
}

// TestRecallIndexCoveredAndText 目录组装：covered 判定（≤ cutoff 已覆盖）、
// 空预览行剔除、关键词过滤、时间/作者展示。
func TestRecallIndexCoveredAndText(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-index"}
	base := time.Now()

	svc.CreateSession(ctx, key, nil)
	sess, _ := svc.GetSession(ctx, key)
	// seq1 在 cutoff 之前（已覆盖），seq2 在 cutoff 之后（未覆盖）
	appendScopedEvents(t, svc, sess, base,
		[3]string{"user", "第一个问题：GLM 的参数量是多少", "https://a.example.com"},
		[3]string{"assistant", "GLM-4 的参数量未公开披露", "https://a.example.com"},
	)

	// cutoff = seq1 时间：seq1 已覆盖、seq2 未覆盖
	text, err := svc.RecallIndex(ctx, key, "https://a.example.com", nil, base)
	if err != nil {
		t.Fatalf("RecallIndex: %v", err)
	}
	if !strings.Contains(text, "#1") || !strings.Contains(text, "已覆盖") ||
		!strings.Contains(text, "#2") || !strings.Contains(text, "未覆盖") {
		t.Fatalf("covered 标注不符：\n%s", text)
	}
	if !strings.Contains(text, "GLM") {
		t.Fatalf("预览缺失：\n%s", text)
	}

	// cutoff 零值（无摘要）→ 全部未覆盖
	text, err = svc.RecallIndex(ctx, key, "https://a.example.com", nil, time.Time{})
	if err != nil {
		t.Fatalf("RecallIndex(zero): %v", err)
	}
	if strings.Count(text, "已覆盖") != 0 {
		t.Fatalf("无摘要应全部未覆盖：\n%s", text)
	}

	// 关键词过滤：命中行保留（“多少”仅出现在 seq1）
	text, err = svc.RecallIndex(ctx, key, "https://a.example.com", []string{"多少"}, time.Time{})
	if err != nil {
		t.Fatalf("RecallIndex(kw): %v", err)
	}
	if !strings.Contains(text, "#1") || strings.Contains(text, "#2") {
		t.Fatalf("关键词过滤不符：\n%s", text)
	}

	// 关键词零命中提示
	text, err = svc.RecallIndex(ctx, key, "https://a.example.com", []string{"不存在词"}, time.Time{})
	if err != nil {
		t.Fatalf("RecallIndex(kw-miss): %v", err)
	}
	if !strings.Contains(text, "没有") {
		t.Fatalf("零命中应返回引导提示：\n%s", text)
	}

	// 仅空白字符的事件（如工具调用前的空 assistant 前导）不进目录
	appendScopedEvents(t, svc, sess, base,
		[3]string{"assistant", "\n", "https://a.example.com"},
	)
	text, err = svc.RecallIndex(ctx, key, "https://a.example.com", nil, time.Time{})
	if err != nil {
		t.Fatalf("RecallIndex(blank): %v", err)
	}
	if strings.Contains(text, "#3") {
		t.Fatalf("空白预览行应被剔除：\n%s", text)
	}
}

// TestRecallFetchBudget fetch 预算：条数上限 12、估算 token 上限、单条超长 rune 截断、
// 超限附提示；空结果空态文案。
func TestRecallFetchBudget(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	base := time.Now()

	// 条数上限：20 条 → 返回前 12 条 + 截断提示
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-budget"}
	svc.CreateSession(ctx, key, nil)
	sess, _ := svc.GetSession(ctx, key)
	evts := make([][3]string, 0, 20)
	for range 20 {
		evts = append(evts, [3]string{"user", "短内容", "https://a.example.com"})
	}
	appendScopedEvents(t, svc, sess, base, evts...)

	text, err := svc.RecallFetch(ctx, key, "https://a.example.com", nil, []int64{1, 20})
	if err != nil {
		t.Fatalf("RecallFetch: %v", err)
	}
	if got := strings.Count(text, "#"); got != 12 {
		t.Fatalf("应返回 12 条，got %d：\n%s", got, text)
	}
	if !strings.Contains(text, "已按预算截断") || !strings.Contains(text, "20") {
		t.Fatalf("缺截断提示：\n%s", text)
	}

	// token 预算（SimpleTokenCounter 4 rune ≈ 1 token，每条 2000 rune ≈ 500 token）：
	// 10 条 → 前 8 条累计 4000 达上限，第 9 条超限被截断
	key2 := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-token"}
	svc.CreateSession(ctx, key2, nil)
	sess2, _ := svc.GetSession(ctx, key2)
	big := strings.Repeat("字", 2000)
	for range 10 {
		appendScopedEvents(t, svc, sess2, base,
			[3]string{"user", big, "https://a.example.com"},
		)
	}
	text, err = svc.RecallFetch(ctx, key2, "https://a.example.com", nil, []int64{1, 10})
	if err != nil {
		t.Fatalf("RecallFetch(token): %v", err)
	}
	if got := strings.Count(text, "#"); got != 8 {
		t.Fatalf("token 预算应返回 8 条，got %d", got)
	}
	if !strings.Contains(text, "已按预算截断") {
		t.Fatalf("token 超限缺截断提示：\n%s", text)
	}

	// 单条超长：rune 截断标记
	key3 := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-huge"}
	svc.CreateSession(ctx, key3, nil)
	sess3, _ := svc.GetSession(ctx, key3)
	appendScopedEvents(t, svc, sess3, base,
		[3]string{"user", strings.Repeat("长", 3000), "https://a.example.com"},
	)
	text, err = svc.RecallFetch(ctx, key3, "https://a.example.com", nil, []int64{1, 1})
	if err != nil {
		t.Fatalf("RecallFetch(huge): %v", err)
	}
	if !strings.Contains(text, "[已截断]") {
		t.Fatalf("单条超长应 rune 截断：\n%s", text)
	}

	// 空结果空态
	text, err = svc.RecallFetch(ctx, key3, "https://a.example.com", nil, []int64{5, 9})
	if err != nil {
		t.Fatalf("RecallFetch(empty): %v", err)
	}
	if !strings.Contains(text, "暂无可回溯历史") {
		t.Fatalf("空结果应返回空态文案：%s", text)
	}

	// 无 keywords 也无 seq_range
	text, err = svc.RecallFetch(ctx, key3, "https://a.example.com", nil, nil)
	if err != nil {
		t.Fatalf("RecallFetch(no-args): %v", err)
	}
	if !strings.Contains(text, "keywords 或 seq_range") {
		t.Fatalf("缺参应返回引导：%s", text)
	}

	// keywords 零命中
	text, err = svc.RecallFetch(ctx, key3, "https://a.example.com", []string{"不存在的词"}, nil)
	if err != nil {
		t.Fatalf("RecallFetch(kw-miss): %v", err)
	}
	if !strings.Contains(text, "零命中") {
		t.Fatalf("keywords 零命中应提示：%s", text)
	}
}

// TestRecallEmptySession 空会话（无事件）的 index/fetch 均返回空态文案，不报错。
func TestRecallEmptySession(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-empty"}

	svc.CreateSession(ctx, key, nil)

	text, err := svc.RecallIndex(ctx, key, "", nil, time.Time{})
	if err != nil || !strings.Contains(text, "暂无可回溯历史") {
		t.Fatalf("空会话 index: text=%q err=%v", text, err)
	}
	text, err = svc.RecallFetch(ctx, key, "", nil, []int64{1, 5})
	if err != nil || !strings.Contains(text, "暂无可回溯历史") {
		t.Fatalf("空会话 fetch: text=%q err=%v", text, err)
	}
}

// TestSummaryCutoffs 覆盖判定数据源：持久化摘要后能读到各 scope 的 CutoffAt；
// 无摘要/会话不存在返回空 map。
func TestSummaryCutoffs(t *testing.T) {
	svc := newRecallService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "recall-cutoffs"}

	// 会话不存在
	if got := svc.SummaryCutoffs(ctx, key); len(got) != 0 {
		t.Fatalf("会话不存在应返回空 map，got %v", got)
	}

	svc.CreateSession(ctx, key, nil)
	if got := svc.SummaryCutoffs(ctx, key); len(got) != 0 {
		t.Fatalf("无摘要应返回空 map，got %v", got)
	}

	cutoff := time.Now().UTC().Truncate(time.Second)
	snapshot := map[string]*session.Summary{
		"https://a.example.com": {
			Summary:   "测试摘要",
			UpdatedAt: cutoff,
			Boundary:  session.NewSummaryBoundaryWithEventID("https://a.example.com", cutoff, ""),
		},
	}
	// 直接走底层 Storage（SaveSummaries 未在 SessionService 门面暴露）
	if err := svc.store.SaveSummaries(ctx, key, snapshot, nil); err != nil {
		t.Fatalf("SaveSummaries: %v", err)
	}
	got := svc.SummaryCutoffs(ctx, key)
	if len(got) != 1 {
		t.Fatalf("应读到 1 个 scope，got %v", got)
	}
	if c, ok := got["https://a.example.com"]; !ok || !c.Equal(cutoff) {
		t.Fatalf("CutoffAt 不符: got %v want %v", c, cutoff)
	}
}
