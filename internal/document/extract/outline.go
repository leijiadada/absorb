// outline.go PDF 原生书签树（/Outlines）提取（F8）：用 pdftohtml -xml 读出
// 书签的层级/标题/页码，替代 docling 模糊匹配产出的断行/错配 Path。
//
// 设计约束（与 poppler.go 双跑同哲学）：
//   - 增强不是依赖——解析失败/pdftohtml 不可用/无书签时返回空，调用方静默
//     回退 docling Path，行为与 F8 前完全一致；
//   - 只产 Path 文案，不产 Offset/Title——Offset/Title 保持 docling 的正文
//     物理行，不变式 text[Offset:] 以 Title 开头零破坏；
//   - 书签页码与 pdftotext 物理页 1-based 对齐（实测主书 page=32 标题就在
//     第 32 页页首），页码→Offset 匹配只在目标页窗口内找，精度高于 docling
//     的 ±2 页窗口。
package extract

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// outlineTimeout 单次 pdftohtml 解析超时（347 页实测 ~5s，取 12 倍裕量）。
const outlineTimeout = 60 * time.Second

// pdftohtmlBin 解析 pdftohtml 可执行文件路径：与 pdftotextBin 同策略
// （显式路径优先，其次 PATH）。可用性与超时独立——缺任一引擎都静默回退。
func pdftohtmlBin() string {
	if p := strings.TrimSpace(os.Getenv("ABSORB_PDFTOHTML")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		return ""
	}
	if path, err := exec.LookPath("pdftohtml"); err == nil {
		return path
	}
	return ""
}

// OutlineItem 一条书签：标题完整文本、层级（1 起）、1-based 物理页码、面包屑链。
type OutlineItem struct {
	Title string
	Level int
	Page  int
	Path  []string
}

// pdf2xml / outline / item 只取 F8 需要的字段；其余 XML 节点（page/fontspec/text）
// 由 encoding/xml 自动忽略。
type pdf2xmlDoc struct {
	XMLName  xml.Name         `xml:"pdf2xml"`
	Outlines []xmlOutlineNode `xml:"outline"`
}

// xmlOutlineNode 用有序子节点捕获 <item>/<outline> 的原始交错顺序——pdftohtml 的
// 书签 XML 里 item 与子 outline 交错出现，子 outline 归属于紧邻其前的那个 item。
// 若拆成 Items/Children 两个 slice 会丢失顺序，导致所有子节错挂到最后一个 item。
// Nodes 捕获本层交错子节点（item 与 outline 按原顺序），walkOutline 按序处理。
type xmlOutlineNode struct {
	XMLName  xml.Name
	Page     int              `xml:"page,attr"`
	Title    string           `xml:",chardata"`
	Nodes    []xmlOutlineNode `xml:",any"`
}
func (n xmlOutlineNode) isItem() bool    { return n.XMLName.Local == "item" }
func (n xmlOutlineNode) isOutline() bool { return n.XMLName.Local == "outline" }

// PopplerOutlines 用 pdftohtml -xml 解析 PDF 原生书签树，返回带层级 Path 的列表。
// 失败（引擎缺失/执行失败/XML 损坏/无书签）返回空切片 + error，调用方回退 docling。
// ctx 取消时立即终止子进程；子进程写临时 PDF，结束即删。
func PopplerOutlines(ctx context.Context, data []byte) ([]OutlineItem, error) {
	bin := pdftohtmlBin()
	if bin == "" || len(data) == 0 {
		return nil, errors.New("outlines: pdftohtml 未配置或输入为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tmp, err := os.CreateTemp("", "absorb-outline-*.pdf")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer func() { tmp.Close(); os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, outlineTimeout)
	defer cancel()
	// -stdout 输出到 stdout；stderr 的告警不进 XML。-hidden 保留隐藏文本节点
	// （书签标题不依赖正文 text 节点，但保留以防个别 PDF 把书签文字标为 hidden）。
	out, err := exec.CommandContext(runCtx, bin, "-xml", "-hidden", "-stdout", tmpPath).Output()
	if err != nil {
		return nil, fmt.Errorf("outlines: pdftohtml 执行失败: %w", err)
	}
	var doc pdf2xmlDoc
	if err := xml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("outlines: XML 解析失败: %w", err)
	}
	var items []OutlineItem
	for _, root := range doc.Outlines {
		walkOutline(root, 1, nil, &items)
	}
	if len(items) == 0 {
		return nil, errors.New("outlines: 无书签项")
	}
	return items, nil
}

// walkOutline 按 Nodes 原顺序遍历一层 <outline>：遇到 <item> 产出一条并以它为
// pendingParent；遇到子 <outline> 则以 pendingParent 为父递归（子 outline 归属
// 紧邻其前的 item）。level 从 1 起，产出的 Path 含自身。
func walkOutline(on xmlOutlineNode, level int, parent []string, out *[]OutlineItem) {
	pending := parent
	for _, node := range on.Nodes {
		switch {
		case node.isItem():
			title := strings.TrimSpace(node.Title)
			if title == "" || node.Page <= 0 {
				continue
			}
			path := make([]string, 0, len(parent)+1)
			path = append(path, parent...)
			path = append(path, title)
			*out = append(*out, OutlineItem{Title: title, Level: level, Page: node.Page, Path: path})
			pending = path // 后续子 outline 归属于此 item
		case node.isOutline():
			walkOutline(node, level+1, pending, out)
		}
	}
}

// outlineMatchSim 书签标题与正文候选行的最小多重集相似度（低于此不覆盖）。
// 0.78 实测：完整标题 vs 断行两行拼接 ≈0.95（多一个章号"1"），vs 单行半句
// ≈0.65（被拒）——既能命中断行对，又不会把半句错配给别的书签。
const outlineMatchSim = 0.78

// outlineMaxWrapLines 书签标题最多跨几行拼接（实测断行标题跨 2 行，留到 3 行裕量）。
const outlineMaxWrapLines = 3

// outlineNearRunes docling mark 与书签匹配行的邻近容差（rune）：两者取自同一份
// 正文但匹配算法不同，行首偏移可能有小差；容差内视为同一标题，覆盖而非新建。
const outlineNearRunes = 40

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ApplyOutlinePaths 用书签完整 Path 覆盖 docling marks 的 Path（F8 hybrid）。
// 只改 Path 文案，Offset/Title 保持 docling 的正文物理行——不变式零破坏。
// 每条书签在自己的页码窗口内找最像的标题行（允许跨行拼接），命中的 mark
// （含断行拆出的两条）全部设为该书签的完整 Path。返回被覆盖的 mark 数。
//
// 页码窗口 ±1 页（实测对齐，留一页裕量抗页号微偏）。匹配不到（相似度不足）
// 的书签跳过，对应 mark 保留 docling Path（前言区 docling-only 天然保留）。
func ApplyOutlinePaths(marks []HeadingMark, outlines []OutlineItem, text string) ([]HeadingMark, int) {
	if len(outlines) == 0 {
		return marks, 0
	}
	rs := []rune(text)
	pages := outlinePageRanges(rs)
	covered := make([]bool, len(marks))
	var added []HeadingMark
	n := 0
	for _, o := range outlines {
		// 页窗口 [o.Page-1, o.Page+1]（1-based → 0-based，±1）
		lo, hi := o.Page-2, o.Page // 0-based 页索引
		if lo < 0 {
			lo = 0
		}
		if hi > len(pages)-1 {
			hi = len(pages) - 1
		}
		for pi := lo; pi <= hi && pi <= len(pages)-1; pi++ {
			bestStart, bestEnd, bestSim := matchOutlineInPage(o.Title, rs, pages[pi].start, pages[pi].end)
			if bestSim < outlineMatchSim {
				continue
			}
			// 命中的 mark（Offset 落在匹配行区间内）全部设为书签完整 Path。
			hit := false
			for i, m := range marks {
				if covered[i] || m.Offset < bestStart || m.Offset >= bestEnd {
					continue
				}
				m.Path = append([]string(nil), o.Path...)
				marks[i] = m
				covered[i] = true
				hit = true
				n++
			}
			// 匹配行区间内无 mark：先在邻近（±outlineNearRunes）找未覆盖的 docling
			// mark 覆盖它（docling 与书签匹配的物理行可能有小偏移），仍无才新建——
			// 避免同一标题既留 docling 旧 mark 又新建书签 mark 造成重复。
			if !hit {
				lineStart := bestStart
				for lineStart > pages[pi].start && rs[lineStart-1] != '\n' && rs[lineStart-1] != '\r' && rs[lineStart-1] != '\f' {
					lineStart--
				}
				near := -1
				for i, m := range marks {
					if covered[i] {
						continue
					}
					d := m.Offset - lineStart
					if d < 0 {
						d = -d
					}
					if d <= outlineNearRunes && (near < 0 || d < absInt(marks[near].Offset-lineStart)) {
						near = i
					}
				}
				if near >= 0 {
					m := marks[near]
					m.Path = append([]string(nil), o.Path...)
					marks[near] = m
					covered[near] = true
					hit = true
					n++
				}
			}
			// 邻近也无 docling mark（该书 docling=0/极少）：用书签直接新建 mark。
			// Title 取匹配区间第一行物理文本（保持不变式 text[Offset:] 以 Title 开头），
			// Offset 取该行行首，Path 用书签完整链。
			if !hit {
				lineEnd := bestStart
				for lineEnd < bestEnd && rs[lineEnd] != '\n' && rs[lineEnd] != '\r' && rs[lineEnd] != '\f' {
					lineEnd++
				}
				lineStart := bestStart
				for lineStart > pages[pi].start && rs[lineStart-1] != '\n' && rs[lineStart-1] != '\r' && rs[lineStart-1] != '\f' {
					lineStart--
				}
				// Offset 去重：已有 mark（含此前新建）已占同一行首则跳过——严格递增
				// 是中点赋值二分的前提，重复 Offset 会破坏检索归属。
				dup := false
				for _, m := range marks {
					if m.Offset == lineStart {
						dup = true
						break
					}
				}
				if !dup {
					for _, m := range added {
						if m.Offset == lineStart {
							dup = true
							break
						}
					}
				}
				if dup {
					break
				}
				title := strings.TrimSpace(string(rs[lineStart:lineEnd]))
				if title != "" {
					lvl := o.Level
					if lvl < 1 || lvl > 6 {
						lvl = 1
					}
					added = append(added, HeadingMark{
						Offset: lineStart, Level: lvl, Title: title,
						Path: append([]string(nil), o.Path...),
					})
					n++
				}
			}
			break // 取相似度达标的首个页窗口，不重复覆盖
		}
	}
	if len(added) > 0 {
		marks = append(marks, added...)
		sort.SliceStable(marks, func(i, j int) bool { return marks[i].Offset < marks[j].Offset })
	}
	return marks, n
}

// pageRange 一页的 rune 区间 [start,end)（不含 \f 本身）。
type pageRange struct{ start, end int }

// outlinePageRanges 按 \f 分页，返回每页 rune 区间（与 pdftotext 物理页 1-based 对齐）。
func outlinePageRanges(rs []rune) []pageRange {
	var out []pageRange
	start := 0
	for i, r := range rs {
		if r == '\f' {
			out = append(out, pageRange{start, i})
			start = i + 1
		}
	}
	out = append(out, pageRange{start, len(rs)})
	return out
}

// matchOutlineInPage 在页内找与标题最像的连续 1~3 行，返回其 rune 区间与相似度。
// 断行标题的两行拼接后与完整书签标题字符集高度重合（含章号"1"等少量额外字符），
// 用多重集相似度容忍这些差异。
func matchOutlineInPage(title string, rs []rune, start, end int) (int, int, float64) {
	if start >= end || end > len(rs) {
		return 0, 0, 0
	}
	// 页内分行（rune 口径，全文偏移）
	type line struct{ s, e int }
	var lines []line
	ls := start
	for i := start; i <= end; i++ {
		if i == end || rs[i] == '\n' || rs[i] == '\r' {
			if i > ls {
				lines = append(lines, line{ls, i})
			}
			ls = i + 1
		}
	}
	bestS, bestE, bestSim := 0, 0, 0.0
	for i := range lines {
		for k := 1; k <= outlineMaxWrapLines && i+k <= len(lines); k++ {
			seg := string(rs[lines[i].s : lines[i+k-1].e])
			if sim := charMultisetSim(title, seg); sim > bestSim {
				bestSim, bestS, bestE = sim, lines[i].s, lines[i+k-1].e
			}
		}
	}
	return bestS, bestE, bestSim
}

// HybridOutlinePaths 用 PDF 原生书签树修正 docling marks 的 Path（F8 入口）。
// content 必须正是 overlayMarks 匹配的同一份文本（marks.Offset 的口径），且含
// \f 分页符——书签页码才能与正文页对齐。switched=false 的主路文本无分页符、
// marks 口径不同，直接跳过回退 docling（避免错位覆盖）。
// marks 可为空——docling 质量门挡掉的书（人月神话/go 程序设计语言）由书签
// 独立生成 marks。返回可能扩充的 marks 与覆盖/新建数；失败时原样返回 marks。
func HybridOutlinePaths(ctx context.Context, marks []HeadingMark, data []byte, content string) ([]HeadingMark, int) {
	if len(data) == 0 || !strings.Contains(content, "\f") {
		return marks, 0 // 无分页：主路文本口径不匹配，回退 docling
	}
	outlines, err := PopplerOutlines(ctx, data)
	if err != nil || len(outlines) == 0 {
		return marks, 0
	}
	return ApplyOutlinePaths(marks, outlines, content)
}
