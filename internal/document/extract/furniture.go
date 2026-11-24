// furniture.go PDF 正文版式家具（页眉/页脚）过滤。
//
// pdftotext 产物按 \f 分页，页眉/页脚逐页重复出现在页首/页尾，混入正文会
// 稀释 BM25 词权重并污染 chunk。纯文本流没有坐标，用首尾行位置近似页眉/
// 页脚区域：
//   - 候选行 = 每页首/尾各 2 行
//   - 指纹 = 去全部空白 + 全角 ASCII 折半角
//   - 同一指纹出现在 >=4 个连续页的同侧候选行 → 判为页眉（页首）/页脚（页尾）
//   - 指纹在任意页的中部行出现 → 排除（正文重复行防误删）
//   - 页码型页脚数字逐页变化，指纹不重复，天然不命中
package extract

import (
	"sort"
	"strings"
	"unicode"
)

// 版式家具阈值。
const (
	furnitureRunPages  = 4 // 邻接规则：同一指纹出现在 >=4 个连续页
	furnitureEdgeLines = 2 // 页首/页尾候选行数（近似顶/底 15% 区域）
)

// furnitureFingerprint 行指纹：去全部空白 + 全角 ASCII（U+FF01-U+FF5E）折半角。
func furnitureFingerprint(text string) string {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hasConsecutiveFurniturePages 页集合中是否存在 >= n 个连续页（页号相邻）。
func hasConsecutiveFurniturePages(pages map[int]bool, n int) bool {
	if len(pages) < n {
		return false
	}
	keys := make([]int, 0, len(pages))
	for p := range pages {
		keys = append(keys, p)
	}
	sort.Ints(keys)
	run := 1
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1]+1 {
			run++
			if run >= n {
				return true
			}
		} else {
			run = 1
		}
	}
	return false
}

// stripPDFFurniture 过滤 \f 分页文本的页眉/页脚行，返回过滤后文本与剔除行数。
// 无 \f 时原样返回；\f 的数量与位置保持不变，只删页内行。
func stripPDFFurniture(text string) (string, int) {
	if !strings.Contains(text, "\f") {
		return text, 0
	}
	pages := strings.Split(text, "\f")

	// 每页行级候选标记：edge=首/尾候选行，headSide=true 表示属于页首侧
	edge := make([][]bool, len(pages))
	headSide := make([][]bool, len(pages))
	headSeen := map[string]map[int]bool{}
	tailSeen := map[string]map[int]bool{}
	bodySeen := map[string]bool{}

	mark := func(seen map[string]map[int]bool, fp string, page int) {
		if seen[fp] == nil {
			seen[fp] = map[int]bool{}
		}
		seen[fp][page] = true
	}

	for pi, page := range pages {
		lines := strings.Split(page, "\n")
		eg := make([]bool, len(lines))
		hs := make([]bool, len(lines))
		edge[pi], headSide[pi] = eg, hs

		var idx []int // 非空行下标（空行无指纹，不参与）
		for i, ln := range lines {
			if strings.TrimSpace(ln) != "" {
				idx = append(idx, i)
			}
		}
		// 极短页（非空行 <= 2×候选行数）不贡献指纹：行太少分不清页眉和正文。
		if len(idx) <= furnitureEdgeLines*2 {
			continue
		}
		headN := furnitureEdgeLines
		tailN := furnitureEdgeLines
		for _, i := range idx[:headN] { // 页首候选
			eg[i], hs[i] = true, true
			if fp := furnitureFingerprint(lines[i]); fp != "" {
				mark(headSeen, fp, pi)
			}
		}
		for _, i := range idx[len(idx)-tailN:] { // 页尾候选
			if eg[i] {
				continue
			}
			eg[i] = true
			if fp := furnitureFingerprint(lines[i]); fp != "" {
				mark(tailSeen, fp, pi)
			}
		}
		for _, i := range idx[headN : len(idx)-tailN] { // 中部行 → 正文区出现
			if fp := furnitureFingerprint(lines[i]); fp != "" {
				bodySeen[fp] = true
			}
		}
	}

	isFurniture := func(seen map[string]map[int]bool, fp string) bool {
		return fp != "" && hasConsecutiveFurniturePages(seen[fp], furnitureRunPages) && !bodySeen[fp]
	}

	removed := 0
	for pi, page := range pages {
		lines := strings.Split(page, "\n")
		kept := make([]string, 0, len(lines))
		for i, ln := range lines {
			if edge[pi][i] {
				fp := furnitureFingerprint(ln)
				seen := tailSeen
				if headSide[pi][i] {
					seen = headSeen
				}
				if isFurniture(seen, fp) {
					removed++
					continue
				}
			}
			kept = append(kept, ln)
		}
		pages[pi] = strings.Join(kept, "\n")
	}
	return strings.Join(pages, "\f"), removed
}
