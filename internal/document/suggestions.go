// suggestions.go 动态建议提问（design/doc-suggestions-design.md）：文档卡就绪
// 态的建议问题由文档自身结构生成，替代写死的 3 问。
//
// 双通道素材（按可用性自动降级，新格式无需新增提问逻辑）：
//   - 通道 A（书型格式）：heading_marks Level≤2 一级/二级章节，按书长
//     25%/55%/85% 位置均匀抽样最多 3 章——模板「书中〈X〉…」；
//   - 通道 B（一切切块产物）：chunk section_path 去重（首现顺序）取末级标题
//     抽最多 3 个小节——模板「文中〈X〉…」。
//
// 通道 A 优先，产出不足走 B；都空返回空（扩展端不渲染建议提问小节）。
// 问题语言跟随文档：服务端中文模板直接组句（与 prepare/knowledge 服务端
// 文案同先例），扩展端原样显示、点击原样发送。
package document

import (
	"context"
	"fmt"
	"sort"

	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store/storage"
)

// 组句模板与条数约束（按通道选择：书中/文中，语义由素材来源决定）。
const (
	suggestBookTpl = "书中〈%s〉这部分讲了什么？作者的核心论据是什么？"
	suggestDocTpl  = "文中〈%s〉这部分讲了什么？作者的核心观点是什么？"
	suggestMax     = 3 // 建议问题条数上限
	suggestMin     = 2 // 低于此条数不产出（宁缺勿凑，不硬凑固定问）
)

// suggestSamplePos 建议抽样位置（书长比例）：跨书首/中/尾，避免全落在文档开头。
var suggestSamplePos = [3]float64{0.25, 0.55, 0.85}

// sectionLoader 通道 B 素材加载接缝（storage.LoadDocumentSections 的方法值，
// 参数化便于单测注入替身）。
type sectionLoader = func(ctx context.Context, app, user, documentID string) ([]string, error)

// buildSuggestions 组建议提问。由 Prepare 在 BuildIndex 之后调用（新建路径已
// 重建、幂等快路径 chunks 已在库），通道 B 的 section_path 查询在该时点可用。
// 返回成句问题（服务端组句，扩展原样显示发送）；无素材返回 nil。
func buildSuggestions(ctx context.Context, app, user, documentID string, dc *storage.DocumentContent, load sectionLoader) []string {
	// 通道 A：heading_marks 层级素材（解析失败按无 marks 走通道 B）
	if qs := suggestFromMarks(parseHeadingMarks(dc.HeadingMarks), dc.CharCount); len(qs) >= suggestMin {
		return qs
	}
	// 通道 B：chunk section_path 平铺素材（索引未就绪/加载失败时为空）
	sections, err := load(ctx, app, user, documentID)
	if err != nil {
		log.Warnf("[Absorb] doc suggestions: load sections failed (doc=%s): %v", documentID, err)
		return nil
	}
	titles := suggestTitlesFromSections(sections)
	if len(titles) < suggestMin {
		return nil
	}
	out := make([]string, 0, len(titles))
	for _, t := range titles {
		out = append(out, fmt.Sprintf(suggestDocTpl, t))
	}
	return out
}

// suggestFromMarks 通道 A：Level≤2 章节按书长位置抽样，各位置取「该位置所在
// 的最后一个章」（sort.Search 二分），去重后按书序输出成句问题。
// total 为书长 rune 数（与 marks.Offset 同口径）；≤0 时以末章 Offset 兜底。
func suggestFromMarks(marks []extract.HeadingMark, total int) []string {
	levels := make([]extract.HeadingMark, 0, len(marks))
	for _, m := range marks {
		if m.Level <= 2 {
			levels = append(levels, m)
		}
	}
	if len(levels) == 0 {
		return nil
	}
	sort.SliceStable(levels, func(i, j int) bool { return levels[i].Offset < levels[j].Offset })
	if total <= 0 {
		total = levels[len(levels)-1].Offset + 1
	}
	seen := make(map[int]bool, len(suggestSamplePos))
	picked := make([]string, 0, len(suggestSamplePos))
	for _, pos := range suggestSamplePos {
		target := int(float64(total) * pos)
		idx := sort.Search(len(levels), func(k int) bool { return levels[k].Offset > target }) - 1
		if idx < 0 {
			idx = 0
		}
		if seen[idx] {
			continue
		}
		seen[idx] = true
		picked = append(picked, fmt.Sprintf(suggestBookTpl, truncateRunes(levels[idx].Title, headingTitleMax)))
	}
	if len(picked) < suggestMin {
		return nil
	}
	return picked
}

// suggestTitlesFromSections 通道 B：section_path → 末级标题（叶子段，取「>」
// 分隔的最后一段——面包屑全串过长且前缀层级与提问无关）。空串已由查询过滤。
func suggestTitlesFromSections(sections []string) []string {
	out := make([]string, 0, suggestMax)
	for _, sp := range sections {
		if leaf := lastSegment(sp); leaf != "" {
			out = append(out, truncateRunes(leaf, headingTitleMax))
		}
		if len(out) >= suggestMax {
			break
		}
	}
	return out
}

// lastSegment 取面包屑末级：「A > B > C」→ "C"；无分隔符返回原串。
func lastSegment(path string) string {
	for i := len(path) - 3; i >= 0; i-- {
		if path[i:i+3] == " > " {
			return path[i+3:]
		}
	}
	return path
}
