package extract

import (
	"context"
	"encoding/xml"
	"reflect"
	"strings"
	"testing"
)

// F8 书签 hybrid 单测（纯函数级，不依赖 pdftohtml/PDF）：分页、断行匹配、
// hybrid 覆盖（只改 Path 不动 Offset/Title）、相似度回退、层级栈。

func TestOutlinePageRanges(t *testing.T) {
	text := "第一页\f第二页\f第三页"
	got := outlinePageRanges([]rune(text))
	want := []pageRange{{0, 3}, {4, 7}, {8, 11}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("page ranges = %v, want %v", got, want)
	}
	// 无分页符：整篇一页
	got = outlinePageRanges([]rune("无分页正文"))
	if len(got) != 1 || got[0] != (pageRange{0, 5}) {
		t.Fatalf("无分页应单页: %v", got)
	}
}

func TestMatchOutlineInPageCrossLine(t *testing.T) {
	// 断行标题：完整书签标题跨两行，含一个额外章号"1"
	page := "1 智能体软件工程：从直\n觉狂欢到可信工程\n开门见山说个残酷现实。"
	rs := []rune(page)
	title := "智能体软件工程：从直觉狂欢到可信工程"
	s, e, sim := matchOutlineInPage(title, rs, 0, len(rs))
	if sim < outlineMatchSim {
		t.Fatalf("断行标题应命中: sim=%.3f < %.2f", sim, outlineMatchSim)
	}
	if got := string(rs[s:e]); !strings.Contains(got, "从直") || !strings.Contains(got, "觉狂欢") {
		t.Fatalf("应覆盖断行两行, got=%q", got)
	}
	// 无关标题不应命中
	_, _, sim2 := matchOutlineInPage("完全不相干的章节名", rs, 0, len(rs))
	if sim2 >= outlineMatchSim {
		t.Fatalf("无关标题不应命中: sim=%.3f", sim2)
	}
}

func TestApplyOutlinePathsHybrid(t *testing.T) {
	// 正文：page1 断行标题（docling 拆两条 mark）+ page2 另一标题
	page1 := "1 智能体软件工程：从直\n觉狂欢到可信工程\n正文内容甲。\f"
	page2 := "2 发挥力量\n正文内容乙。"
	text := page1 + page2
	rs := []rune(text)
	runeOff := func(sub string) int { return len([]rune(text[:strings.Index(text, sub)])) }

	// docling marks：断行两条（Title 为物理行），page2 一条
	marks := []HeadingMark{
		{Offset: runeOff("1 智能体"), Level: 1, Title: "1 智能体软件工程：从直", Path: []string{"1 智能体软件工程：从直"}},
		{Offset: runeOff("觉狂欢到可信工程"), Level: 1, Title: "觉狂欢到可信工程", Path: []string{"觉狂欢到可信工程"}},
		{Offset: runeOff("2 发挥力量"), Level: 1, Title: "2 发挥力量", Path: []string{"2 发挥力量"}},
	}
	origTitle := []string{marks[0].Title, marks[1].Title, marks[2].Title}
	origOff := []int{marks[0].Offset, marks[1].Offset, marks[2].Offset}

	// 书签：page1 完整标题（应覆盖断行两条），page2 title 带差异（应覆盖第三条）
	outlines := []OutlineItem{
		{Title: "智能体软件工程：从直觉狂欢到可信工程", Level: 1, Page: 1,
			Path: []string{"智能体软件工程：从直觉狂欢到可信工程"}},
		{Title: "2 发挥力量", Level: 1, Page: 2, Path: []string{"发挥 AI 队友的力量", "2 发挥力量"}},
	}
	marks, covered := ApplyOutlinePaths(marks, outlines, text)
	if covered != 3 {
		t.Fatalf("应覆盖 3 条 mark, got %d", covered)
	}
	// 不变式：Offset/Title 逐位不变
	for i, m := range marks {
		if m.Offset != origOff[i] || m.Title != origTitle[i] {
			t.Fatalf("mark[%d] Offset/Title 被改动: %d/%q", i, m.Offset, m.Title)
		}
	}
	// Path 被书签完整标题覆盖（断行两条都变完整）
	if !reflect.DeepEqual(marks[0].Path, outlines[0].Path) || !reflect.DeepEqual(marks[1].Path, outlines[0].Path) {
		t.Fatalf("断行两条应覆盖为完整 Path: %v / %v", marks[0].Path, marks[1].Path)
	}
	if !reflect.DeepEqual(marks[2].Path, outlines[1].Path) {
		t.Fatalf("page2 Path 应为书签多级链: %v", marks[2].Path)
	}
	// 原文不变式：每个 Offset 处仍以原 Title 开头
	for i, m := range marks {
		if !strings.HasPrefix(string(rs[m.Offset:]), m.Title) {
			t.Fatalf("不变式破坏 mark[%d]: text[%d:] 不以 %q 开头", i, m.Offset, m.Title)
		}
	}
}

func TestApplyOutlinePathsNoMatchKeepsDocling(t *testing.T) {
	text := "第一章 需求工程\n正文内容。\f第二章 设计\n更多正文。"
	marks := []HeadingMark{
		{Offset: 0, Level: 1, Title: "第一章 需求工程", Path: []string{"第一章 需求工程"}},
		{Offset: len([]rune("第一章 需求工程\n正文内容。\f")), Level: 1, Title: "第二章 设计", Path: []string{"第二章 设计"}},
	}
	orig := make([][]string, len(marks))
	for i, m := range marks {
		orig[i] = append([]string(nil), m.Path...)
	}
	// 书签标题完全不匹配 → 不覆盖
	outlines := []OutlineItem{
		{Title: "毫不相关的标题XYZ", Level: 1, Page: 1, Path: []string{"毫不相关的标题XYZ"}},
	}
	if _, n := ApplyOutlinePaths(marks, outlines, text); n != 0 {
		t.Fatalf("不匹配应 0 覆盖, got %d", n)
	}
	for i, m := range marks {
		if !reflect.DeepEqual(m.Path, orig[i]) {
			t.Fatalf("mark[%d] Path 应保留 docling: %v", i, m.Path)
		}
	}
}

func TestWalkOutlineNesting(t *testing.T) {
	// 按 pdftohtml XML 原顺序：item 与其子 outline 交错出现。
	root := xmlOutlineNode{Nodes: []xmlOutlineNode{
		{XMLName: xml.Name{Local: "item"}, Page: 1, Title: "第一章"},
		{XMLName: xml.Name{Local: "outline"}, Nodes: []xmlOutlineNode{
			{XMLName: xml.Name{Local: "item"}, Page: 2, Title: "1.1 概述"},
			{XMLName: xml.Name{Local: "outline"}, Nodes: []xmlOutlineNode{
				{XMLName: xml.Name{Local: "item"}, Page: 3, Title: "1.1.1 细节"},
			}},
		}},
	}}
	var out []OutlineItem
	walkOutline(root, 1, nil, &out)
	if len(out) != 3 {
		t.Fatalf("应有 3 条书签, got %d: %v", len(out), out)
	}
	wantPaths := [][]string{
		{"第一章"},
		{"第一章", "1.1 概述"},
		{"第一章", "1.1 概述", "1.1.1 细节"},
	}
	wantLevels := []int{1, 2, 3}
	for i := range out {
		if !reflect.DeepEqual(out[i].Path, wantPaths[i]) || out[i].Level != wantLevels[i] {
			t.Fatalf("bookmark[%d] = %v L%d, want %v L%d", i, out[i].Path, out[i].Level, wantPaths[i], wantLevels[i])
		}
	}
}

func TestHybridOutlinePathsNoPaginationFallback(t *testing.T) {
	// 主路无分页文本：应静默回退（返回 0），不调 pdftohtml。
	marks := []HeadingMark{{Offset: 0, Level: 1, Title: "标题", Path: []string{"标题"}}}
	if _, n := HybridOutlinePaths(context.Background(), marks, []byte("dummy"), "无分页正文"); n != 0 {
		t.Fatalf("无分页应回退 0, got %d", n)
	}
}
