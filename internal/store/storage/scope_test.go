package storage

// scope 标识（定长哈希）性质测试：
//  1. ScopeID 确定性、定长、纯十六进制、不含 "/"；
//  2. URL 归一变体（utm/fragment/尾斜杠/query 顺序/host 大小写）同标识；
//  3. 不同文章不同标识；
//  4. 前缀退化：两个不同标识互不为 "/" 前缀——
//     event.Filter 与 SummaryFilterKeyMatchesPrefix 在标识上退化为精确匹配。

import (
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

func TestScopeID_DeterministicAndFixedShape(t *testing.T) {
	url := "https://weknora.weixin.qq.com/docs/03-features/04-chunking"
	id1, id2 := ScopeID(url), ScopeID(url)
	if id1 != id2 {
		t.Fatalf("ScopeID 不确定性：%q != %q", id1, id2)
	}
	if len(id1) != ScopeIDLen {
		t.Fatalf("标识长度=%d, want %d（必须定长，不等长会复活前缀问题）", len(id1), ScopeIDLen)
	}
	if strings.ContainsAny(id1, "/\\ \t") {
		t.Fatalf("标识含路径分隔符或空白：%q", id1)
	}
	for _, c := range id1 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("标识非小写十六进制：%q", id1)
		}
	}
	if ScopeID("") != "" || ScopeID("   ") != "" {
		t.Fatal("空输入应返回空串（不打标）")
	}
}

func TestScopeID_NormalizationVariantsUnify(t *testing.T) {
	base := ScopeID("https://b.com/post/1")
	variants := []string{
		"https://B.com/post/1?utm_source=twitter#top",
		"https://b.com/post/1/?utm_source=wechat",
		"https://b.com/post/1?utm_campaign=x&utm_source=y#comments",
	}
	for _, v := range variants {
		if got := ScopeID(v); got != base {
			t.Fatalf("归一变体应同标识：%q → %q, want %q", v, got, base)
		}
	}
	// query 顺序差异（utm 之外的参数排序）
	if ScopeID("https://b.com/s?x=1&y=2") != ScopeID("https://b.com/s?y=2&x=1") {
		t.Fatal("query 参数顺序差异应同标识")
	}
	// 非 URL 形态原样哈希（确定性即可）
	got1, got2 := ScopeID("not a url"), ScopeID("not a url")
	if got1 != got2 {
		t.Fatal("非 URL 输入也应确定")
	}
}

func TestScopeID_DistinctArticlesDistinctIDs(t *testing.T) {
	// 尾斜杠差异是同一篇文章（归一抹平），不进碰撞集合
	if ScopeID("https://a.com") != ScopeID("https://a.com/") {
		t.Fatal("尾斜杠差异应同标识")
	}
	seen := make(map[string]string)
	for _, u := range []string{
		"https://a.com",       // 根
		"https://a.com/x",     // 子路径
		"https://a.com/x?y=1", // 带 query
		"https://ab.com",      // 前缀相近的不同 host
		"https://b.com/x",     // 不同 host 同路径
		"http://a.com/x",      // scheme 差异（归一后仍不同：归一只小写 scheme，不统一 scheme）
	} {
		id := ScopeID(u)
		if prev, dup := seen[id]; dup {
			t.Fatalf("标识碰撞：%q 与 %q 同为 %q", u, prev, id)
		}
		seen[id] = u
	}
}

// TestScopeID_PrefixRelationImpossible 核心性质：任意两个不同标识
// 互不为 "/" 前缀。由此 SDK 的三条层级路径全部退化为精确匹配：
//   - event.Filter（双向前缀）→ 仅相等为真；
//   - session.SummaryFilterKeyMatchesPrefix（摘要聚合）→ 仅相等为真；
//   - filterEventsInScope / matchSummaryFilterKey（SDK 内部 scope 过滤）同理。
//
// 由此根治：目录页提问不再聚合子孙摘要，也不再按全会话下标 cutoff 裁掉自己的历史。
func TestScopeID_PrefixRelationImpossible(t *testing.T) {
	parent := ScopeID("https://site.com/docs")            // 目录页
	child := ScopeID("https://site.com/docs/03-features") // 子孙文章
	sibling := ScopeID("https://site.com/docs/05-retrieval")

	for _, other := range []string{child, sibling} {
		if other == parent {
			t.Fatal("不同网址不应同标识")
		}
		// 模拟 SummaryFilterKeyMatchesPrefix 的判定式
		if other == parent || strings.HasPrefix(other+"/", parent+"/") {
			t.Fatalf("标识 %q 不应匹配目录页前缀 %q（前缀关系必须不可能）", other, parent)
		}
		// event.Filter：不同标识互不匹配（Version 置当前版本才会读 FilterKey）
		evt := event.Event{Version: event.CurrentVersion, FilterKey: other}
		if evt.Filter(parent) {
			t.Fatalf("event.Filter(%q) 对 %q 应为 false", parent, other)
		}
	}
	// 自身精确匹配保持为真（同文章重放/摘要命中不受影响）
	self := event.Event{Version: event.CurrentVersion, FilterKey: parent}
	if !self.Filter(parent) {
		t.Fatal("event.Filter 对自身标识应为 true")
	}
	// SDK 摘要聚合判定：仅相等命中
	if session.SummaryFilterKeyMatchesPrefix(child, parent) {
		t.Fatal("SummaryFilterKeyMatchesPrefix(子孙, 目录页) 应为 false（聚合不再误触发）")
	}
	if !session.SummaryFilterKeyMatchesPrefix(parent, parent) {
		t.Fatal("SummaryFilterKeyMatchesPrefix 对自身应为 true（精确摘要仍可命中）")
	}
	// 无标事件（FilterKey=""）仍被每个 scope 纳入（语义不变）
	untagged := event.Event{Version: event.CurrentVersion}
	if !untagged.Filter(parent) {
		t.Fatal("无标事件应仍被纳入每个 scope")
	}
}
