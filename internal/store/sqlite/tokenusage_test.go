package sqlite

// token_usage（0006）+ TokenUsageStats 聚合查询（telemetry 第二批）：今日/全期/按 kind
// 分布与缓存命中率的口径验证，及空表除零安全。

import (
	"context"
	"testing"
	"time"

	"absorb-agent/internal/store/storage"
)

func TestTokenUsageStatsEmpty(t *testing.T) {
	st, _ := openStore(t, "tokenstats-empty.db")
	res, err := st.TokenUsageStats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if res.Today.Calls != 0 || res.Today.TotalTokens != 0 || res.Total.Calls != 0 || res.Total.TotalTokens != 0 {
		t.Fatalf("empty stats = %+v, want zeros", res)
	}
	if res.CacheHitRate != 0 {
		t.Fatalf("cacheHitRate = %v, want 0 (除零安全)", res.CacheHitRate)
	}
	if res.ByKind == nil || len(res.ByKind) != 0 {
		t.Fatalf("byKind = %+v, want 空 map（非 nil，前端拿到 {} 而非 null）", res.ByKind)
	}
}

func TestTokenUsageStatsAggregates(t *testing.T) {
	st, _ := openStore(t, "tokenstats.db")
	ctx := context.Background()

	// 今日行锚定当日 0 点后 30 秒起（不受运行时刻影响，必落今日窗口）；
	// 历史行用固定时间戳（2024-10），用于验证跨日累计口径。
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	old := time.Unix(1730000000, 0)

	recs := []storage.TokenUsageRecord{
		{TS: midnight.Add(30 * time.Second), Kind: "chat", Model: "m1",
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CachedTokens: 40},
		{TS: midnight.Add(90 * time.Second), Kind: "chat", Model: "m1",
			PromptTokens: 200, CompletionTokens: 100, ReasoningTokens: 10, TotalTokens: 300, CachedTokens: 60},
		{TS: midnight.Add(150 * time.Second), Kind: "draft", Model: "m1",
			PromptTokens: 50, CompletionTokens: 25, TotalTokens: 75},
		{TS: old, Kind: "chat", Model: "m0",
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	for i, r := range recs {
		if err := st.InsertTokenUsage(ctx, r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	res, err := st.TokenUsageStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	// 今日：3 次调用（不含历史行），prompt=350 cached=100 → 命中率 100/350 ≈ 0.2857
	if res.Today.Calls != 3 {
		t.Fatalf("today calls = %d, want 3", res.Today.Calls)
	}
	if res.Today.PromptTokens != 350 || res.Today.CompletionTokens != 175 ||
		res.Today.TotalTokens != 525 || res.Today.CachedTokens != 100 {
		t.Fatalf("today totals = %+v, want prompt=350 completion=175 total=525 cached=100", res.Today)
	}
	if res.CacheHitRate < 0.2857 || res.CacheHitRate > 0.2858 {
		t.Fatalf("cacheHitRate = %v, want 100/350 ≈ 0.2857", res.CacheHitRate)
	}

	// 全部累计：4 次调用含历史行
	if res.Total.Calls != 4 || res.Total.TotalTokens != 540 {
		t.Fatalf("total = %+v, want calls=4 totalTokens=540", res.Total)
	}

	// 按 kind（全期）：chat=3（今日 2 + 历史 1）、draft=1；summary 未上报则无键
	if got, ok := res.ByKind["chat"]; !ok || got.Calls != 3 || got.TotalTokens != 465 {
		t.Fatalf("byKind chat = %+v, want calls=3 totalTokens=465", got)
	}
	if got, ok := res.ByKind["draft"]; !ok || got.Calls != 1 || got.TotalTokens != 75 {
		t.Fatalf("byKind draft = %+v, want calls=1 totalTokens=75", got)
	}
	if _, ok := res.ByKind["summary"]; ok {
		t.Fatalf("byKind 不应包含未上报的 summary 键")
	}
}
