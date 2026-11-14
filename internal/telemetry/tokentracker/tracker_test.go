package tokentracker

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// atFixed 构造返回固定本地时刻的时间源（按日聚合测试用）。
func atFixed(y int, mo time.Month, d int) func() time.Time {
	return func() time.Time { return time.Date(y, mo, d, 12, 0, 0, 0, time.Local) }
}

// TestRecordSnapshotAggregation 聚合正确性：按日/kind/model 三个维度分桶累加，
// Snapshot 按 day/kind/model 升序稳定输出。
func TestRecordSnapshotAggregation(t *testing.T) {
	tr := NewTracker()

	// 第一天：chat 同一 (kind,model) 两次累加
	tr.now = atFixed(2026, 1, 1)
	tr.record(KindChat, "glm-4.7", Usage{PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 5, TotalTokens: 120, CachedTokens: 30})
	tr.record(KindChat, "glm-4.7", Usage{PromptTokens: 50, CompletionTokens: 10, ReasoningTokens: 2, TotalTokens: 60, CachedTokens: 10})
	// 第一天：draft 另一 kind、summary 另一 model
	tr.record(KindDraft, "glm-4.7", Usage{PromptTokens: 200, CompletionTokens: 30, TotalTokens: 230})
	tr.record(KindSummary, "glm-4.7-flash", Usage{PromptTokens: 400, CachedTokens: 100})

	// 第二天：chat 另一日期
	tr.now = atFixed(2026, 1, 2)
	tr.record(KindChat, "glm-4.7", Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11})

	got := tr.Snapshot()
	want := []UsageStat{
		{Day: "2026-01-01", Kind: KindChat, Model: "glm-4.7", Calls: 2, PromptTokens: 150, CompletionTokens: 30, ReasoningTokens: 7, TotalTokens: 180, CachedTokens: 40},
		{Day: "2026-01-01", Kind: KindDraft, Model: "glm-4.7", Calls: 1, PromptTokens: 200, CompletionTokens: 30, TotalTokens: 230},
		{Day: "2026-01-01", Kind: KindSummary, Model: "glm-4.7-flash", Calls: 1, PromptTokens: 400, TotalTokens: 400, CachedTokens: 100},
		{Day: "2026-01-02", Kind: KindChat, Model: "glm-4.7", Calls: 1, PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
	}
	if len(got) != len(want) {
		t.Fatalf("Snapshot len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Snapshot[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestTotalFallback 端点未回 total 时用 prompt+completion 兜底（summary 场景 completion
// 恒缺省，兜底后 total=prompt）。
func TestTotalFallback(t *testing.T) {
	tr := NewTracker()
	tr.now = atFixed(2026, 1, 1)
	tr.record(KindChat, "m", Usage{PromptTokens: 30, CompletionTokens: 12})
	tr.record(KindSummary, "m", Usage{PromptTokens: 40})

	got := tr.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(got))
	}
	if got[0].TotalTokens != 42 {
		t.Errorf("chat total = %d, want 42 (30+12)", got[0].TotalTokens)
	}
	if got[1].TotalTokens != 40 {
		t.Errorf("summary total = %d, want 40 (prompt 兜底)", got[1].TotalTokens)
	}
}

// TestRecordConcurrency 并发 Record 的聚合正确性：多 goroutine 并发写同一桶，
// 累加结果与串行求和一致。
func TestRecordConcurrency(t *testing.T) {
	tr := NewTracker()
	tr.now = atFixed(2026, 1, 1)

	const goroutines, perG = 8, 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				tr.record(KindChat, "m", Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3, CachedTokens: 1})
			}
		}(g)
	}
	wg.Wait()

	got := tr.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(got))
	}
	n := goroutines * perG
	b := got[0]
	if b.Calls != n || b.PromptTokens != 2*n || b.CompletionTokens != n || b.TotalTokens != 3*n || b.CachedTokens != n {
		t.Errorf("aggregate = %+v, want calls=%d prompt=%d completion=%d total=%d cached=%d",
			b, n, 2*n, n, 3*n, n)
	}
}

// TestSummary 今日汇总只累计本地今天的桶（跨日桶不计入）。
func TestSummary(t *testing.T) {
	tr := NewTracker()
	tr.now = atFixed(2026, 1, 1)
	tr.record(KindChat, "m", Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CachedTokens: 3})
	tr.now = atFixed(2026, 1, 2)
	tr.record(KindChat, "m", Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9, CachedTokens: 1})

	s := tr.Summary() // now = 1 月 2 日
	for _, want := range []string{"total=9", "prompt=7", "completion=2", "cached=1"} {
		if !strings.Contains(s, want) {
			t.Errorf("Summary = %q, want contains %q", s, want)
		}
	}
}

// TestSink 异步 sink 收到完整流水记录（main 装配 sqlite sink 的行为契约）。
func TestSink(t *testing.T) {
	tr := NewTracker()
	tr.now = atFixed(2026, 1, 1)
	ch := make(chan Record, 1)
	tr.sink = func(rec Record) { ch <- rec }

	tr.record(KindDraft, "glm-4.7", Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18, CachedTokens: 2})

	select {
	case rec := <-ch:
		if rec.Kind != KindDraft || rec.Model != "glm-4.7" ||
			rec.PromptTokens != 11 || rec.CompletionTokens != 7 ||
			rec.TotalTokens != 18 || rec.CachedTokens != 2 {
			t.Errorf("sink record = %+v", rec)
		}
		if rec.TS.IsZero() {
			t.Errorf("sink record TS not filled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sink not called within timeout")
	}
}
