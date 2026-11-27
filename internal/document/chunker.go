// chunker（M1）：document_contents.content_text → 检索片段。
// 设计约束（doc2 §3.2）：偏移量是唯一可靠定位手段（提取器无页码/章节），
// 因此 Chunk 的 [StartOffset,EndOffset) 必须满足 text[o.Start:o.End] == o.Content
// （rune 偏移，与 CharCount 同口径）。v4 起 section_path 承载标题面包屑——
// 仅 HTML(Markdown) 提取产物含 # 标题行；PDF 纯文本自动降级为无面包屑窗口切块。
package document

import "strings"

// ChunkerVersion 切块规则版本：窗口/边界策略变化时 +1，触发全量索引重建。
// v2（2026-11-15）：目标块 800→400——bge 系 embedding 训练上下文 512 token
// （中文 ≈1 token/字），块超限时尾部内容不进向量（llama-server 直接拒绝超长
// 输入，客户端截断兜底），语义检索对块尾部信息天然失明；块尺寸对齐模型上下文。
// v3（2026-11-17）：断点优先级细化为 空行(\n\n) > 单换行 > 句末标点——此前
// 单换行与空行同级（注释与实现不符）。HTML(Markdown) 与 PDF 两条提取路径的
// 段落/页界信号都是空行，单换行仅代表普通结构行；断点优先落真正的段落边界，
// 改善页界与 Markdown 分段的切块质量。
// v4（2026-11-17，借鉴 WeKnora）：标题感知切块——围栏外 ATX 标题行开启分区，
// ≤目标块的分区整段成块且 SectionPath 记标题面包屑（向量化输入前置面包屑，
// 检索召回携带章节语境）；超长分区退回滑动窗口。围栏代码块为保护区：不在代码
// 行中间切割、围栏内 # 不误判标题；重叠起点向前吸附到最近自然边界。
// 无标题文本（PDF 提取产物）行为与 v3 一致。
// v5（2026-11-18）：bge 移除、语义路换 Qwen3-Embedding-4B（32K token 窗口）
// 后目标块 400→900——400 约束随 bge 消失；48 题 22 个全 miss 的失败模式是
// 「查询与锚点无共同字眼，只能靠语义路桥接」，而语义路的原料是块内上下文
// 完整性（400~800 字小节被滑窗切碎）。900（≈600~900 token，占窗口 2~3%）
// 让绝大多数书籍小节整段成块；1500+ 语义稀释风险大不采用。配套参数：
// 重叠/碎块阈值/断点回看/吸附预算按窗口比例放大。若 900 仍不足，后备方案
// 父子分块（1500 父 + 400 子）另行评估。
// v5（2026-11-18）：bge 移除、语义路换 Qwen3-Embedding-4B（32K token 窗口）
// 后目标块 400→900——400 约束随 bge 消失；48 题 22 个全 miss 的失败模式是
// 「查询与锚点无共同字眼，只能靠语义路桥接」，而语义路的原料是块内上下文
// 完整性（400~800 字小节被滑窗切碎）。900（≈600~900 token，占窗口 2~3%）
// 让绝大多数书籍小节整段成块。1200 档按预注册规则两轮对照未达标（均值
// Recall@10 54.5%<57%、MRR 0.37<0.46，单轮 57% 峰值系噪声），维持 900。
// 后备方案父子分块（1500 父+400 子）另行评估。
const ChunkerVersion = 5

const (
	chunkTargetRunes  = 900 // 目标块长（rune）：≈600~900 token（Qwen3 中文压缩），整节成块承载完整论证链
	chunkOverlapRunes = 180 // 相邻块重叠：目标块的 20%，跨块边界的句子不因切割而检索丢失
	minChunkRunes     = 160 // 碎块合并阈值：块变大后避免产出只有一两句的碎块
	breakLookback     = 450 // 断句点向前搜索的最大距离（半个窗口内必有自然边界）
	overlapSnapMax    = 72  // 重叠起点向前吸附的最大距离：更远保持原位（重叠预算充裕）
	sectionPathMax    = 96  // 面包屑总长上限（rune）：900 块体 + 96 + 1 ≈ 1000，Qwen3 窗口内无截断
	headingTitleMax   = 40  // 单级标题文本上限（rune）
)

// textChunk 切块产物：原文 + rune 偏移 + 生效标题面包屑。
type textChunk struct {
	Content     string
	StartOffset int
	EndOffset   int
	SectionPath string
}

// 断点优先级（数值大者胜，同级取窗口内最靠右的）：
// 空行（连续换行 run，段落/页界）> 单换行（列表项/表格行/硬换行）> 句末标点。
// 空行不经此表：findBreak 对 '\n' 特判直接返回。
const (
	prioLineBreak = 2
	prioSentence  = 1
)

// breakRunes 单字符断点；空行不是单字符断点，由 findBreak 对连续 '\n' 特判识别。
var breakRunes = map[rune]int{
	'\n': prioLineBreak,
	'。':  prioSentence, '！': prioSentence, '？': prioSentence,
	'!': prioSentence, '?': prioSentence, '；': prioSentence, ';': prioSentence,
}

// heading 围栏外的 ATX 标题行事件。
type heading struct {
	offset int // 行首 rune 偏移（分区起点）
	level  int // 1..6
	title  string
}

// fenceSpan 围栏代码保护区 [start, end)（含围栏行）：区内不在行中间切割。
type fenceSpan struct{ start, end int }

// ChunkText 把正文切成带偏移的检索片段：有标题时按标题分区（整段成块 +
// 面包屑，超长分区退回滑动窗口），无标题时整体滑动窗口；断点优先自然边界
// （空行/换行/句末），无边界才硬切；相邻块重叠且起点吸附语义边界；碎块并入邻块。
func ChunkText(text string) []textChunk {
	rs := []rune(text)
	n := len(rs)
	if n == 0 {
		return nil
	}

	headings, fences := scanStructure(rs)
	var out []textChunk
	if len(headings) == 0 {
		out = chunkByWindow(rs, 0, n, "", fences)
	} else {
		out = chunkBySections(rs, headings, fences)
	}
	return coalesceTiny(rs, out)
}

// chunkBySections 标题分区切块：每个标题开启一个分区，相邻分区面包屑同父且
// 累计 ≤ 目标块的装箱合并（块取首分区面包屑），超长分区退回滑动窗口。
func chunkBySections(rs []rune, headings []heading, fences []fenceSpan) []textChunk {
	paths := breadcrumbPaths(headings)
	type sect struct {
		start, end int
		path       string
	}
	secs := make([]sect, 0, len(headings))
	for k, h := range headings {
		end := len(rs)
		if k+1 < len(headings) {
			end = headings[k+1].offset
		}
		secs = append(secs, sect{h.offset, end, paths[k]})
	}

	var out []textChunk
	if lead := headings[0].offset; lead > 0 { // 导语区独立成段（空面包屑）：
		// 不与首个标题分区合并，否则合并块会继承空路径、丢失章节语境
		out = append(out, chunkByWindow(rs, 0, lead, "", fences)...)
	}

	parent := func(path string) string {
		if i := strings.LastIndex(path, " > "); i >= 0 {
			return path[:i]
		}
		return ""
	}
	packStart, packPath, packLen := 0, "", -1 // packLen<0 = 空 pack
	flush := func(to int) {
		if to > packStart {
			out = append(out, chunkByWindow(rs, packStart, to, packPath, fences)...)
		}
	}
	for _, s := range secs {
		l := s.end - s.start
		if packLen >= 0 && (packLen+l > chunkTargetRunes || parent(s.path) != parent(packPath)) {
			flush(s.start)
			packLen = -1
		}
		if packLen < 0 {
			packStart, packPath, packLen = s.start, s.path, 0
		}
		packLen += l
	}
	flush(len(rs))
	return out
}

// chunkByWindow 滑动窗口切块（v3 算法 + 保护区约束 + 重叠吸附），限定 [lo, hi)
// 内：块尾优先自然断点，重叠回退并向语义边界吸附，块尾剩余不足目标直接收尾。
func chunkByWindow(rs []rune, lo, hi int, path string, fences []fenceSpan) []textChunk {
	var out []textChunk
	start := lo
	for start < hi {
		end := start + chunkTargetRunes
		if end >= hi {
			out = append(out, textChunk{string(rs[start:hi]), start, hi, path})
			break
		}
		brk := findBreak(rs, start, end, fences)
		if brk <= start {
			brk = end
		}
		out = append(out, textChunk{string(rs[start:brk]), start, brk, path})
		// 重叠回退：但不小于 start+1，保证单调前进；重叠起点落在词/句中间
		// 会割裂语义，向前吸附到最近自然边界（吸附量计入重叠预算）。
		next := brk - chunkOverlapRunes
		if next <= start {
			next = start + 1
		} else {
			next = snapToBreak(rs, next, brk, fences)
		}
		start = next
	}
	return out
}

// coalesceTiny 碎块归并（含原尾块合并语义）：小于 minChunkRunes 的块并入邻块
// ——当前块过小并入前块（面包屑保留前块），前块过小被当前块吸收（面包屑保留
// 当前块）。覆盖装箱边界处面包屑不同导致无法预合并的孤立小区。
func coalesceTiny(rs []rune, chunks []textChunk) []textChunk {
	var out []textChunk
	for _, c := range chunks {
		if n := len(out); n == 0 {
			out = append(out, c)
			continue
		} else if out[n-1].EndOffset-out[n-1].StartOffset < minChunkRunes {
			c.StartOffset = out[n-1].StartOffset // 前块过小：被当前块吸收
			c.Content = string(rs[c.StartOffset:c.EndOffset])
			out[n-1] = c
			continue
		} else if c.EndOffset-c.StartOffset < minChunkRunes {
			out[n-1].EndOffset = c.EndOffset // 当前块过小：并入前块
			out[n-1].Content = string(rs[out[n-1].StartOffset:out[n-1].EndOffset])
			continue
		}
		out = append(out, c)
	}
	return out
}

// findBreak 在 [end-breakLookback, end) 内倒扫最靠后的自然断点，优先级
// 空行（连续换行 run）> 单换行 > 句末标点，同级取最靠右；空行命中即返回。
// 保护区（围栏）内仅接受换行候选——代码行中间的标点/空行语义不可靠，只能
// 沿行边界切割。返回断点后的位置（断点字符归当前块；空行吞入整个换行 run）。
func findBreak(rs []rune, start, end int, fences []fenceSpan) int {
	lo := end - breakLookback
	if lo < start {
		lo = start
	}
	best := -1
	bestPrio := 0
	for i := end - 1; i >= lo; i-- {
		if inFence(i, fences) && rs[i] != '\n' {
			continue
		}
		if rs[i] == '\n' {
			// 空行判定必须先于同级门控：右侧已有单换行（prio 相同）时，
			// 更靠左的空行仍应胜出。
			if brk := blankLineBreak(rs, i); brk >= 0 {
				return brk // 空行（段落/页界）已是最高优先级，直接采用
			}
			if prioLineBreak <= bestPrio {
				continue // 单换行同级取最靠右
			}
			best = i + 1
			bestPrio = prioLineBreak
			continue
		}
		prio, ok := breakRunes[rs[i]]
		if !ok || prio <= bestPrio {
			continue
		}
		best = i + 1 // 断点字符算进当前块
		bestPrio = prio
	}
	return best
}

// blankLineBreak 判断 i 处 '\n' 是否处于空行 run（连续 ≥2 个 '\n'）：
// 是则返回断点位置（run 最后一个 '\n' 的下一索引，整个 run 归当前块）；否则 -1。
func blankLineBreak(rs []rune, i int) int {
	if i+1 >= len(rs) || rs[i+1] != '\n' {
		return -1
	}
	j := i + 1
	for j+1 < len(rs) && rs[j+1] == '\n' {
		j++
	}
	return j + 1
}

// snapToBreak 重叠起点吸附：在 [from, from+overlapSnapMax) ∩ [from, limit) 内
// 向前找最近自然边界（换行优先，句末标点就近）；无则保持原位。limit 防止
// 吸附越过当前断点。
func snapToBreak(rs []rune, from, limit int, fences []fenceSpan) int {
	max := from + overlapSnapMax
	if max > limit {
		max = limit
	}
	for i := from; i < max; i++ {
		if inFence(i, fences) && rs[i] != '\n' {
			continue
		}
		if rs[i] == '\n' {
			return i + 1 // 行边界：强吸附
		}
		if _, ok := breakRunes[rs[i]]; ok {
			return i + 1 // 句末标点：就近吸附
		}
	}
	return from
}

// inFence 判断偏移是否落在围栏保护区内部。
func inFence(i int, fences []fenceSpan) bool {
	for _, f := range fences {
		if i >= f.start && i < f.end {
			return true
		}
	}
	return false
}

// scanStructure 单遍行扫描：产出围栏外的 ATX 标题事件与围栏保护区区间。
// 围栏 = ≥3 个反引号开头的行（CommonMark：信息串与闭合行内不得再含反引号），
// 开/闭交替；未闭合围栏延伸到文末。
func scanStructure(rs []rune) ([]heading, []fenceSpan) {
	var headings []heading
	var fences []fenceSpan
	n := len(rs)
	i := 0
	inFence := false
	for i < n {
		lineStart := i
		j := i
		for j < n && rs[j] != '\n' {
			j++
		}
		lineEnd := j // 不含换行
		k := lineStart
		for k < lineEnd && (rs[k] == ' ' || rs[k] == '\t') {
			k++
		}
		if k < lineEnd && rs[k] == '`' {
			ticks := 0
			for k+ticks < lineEnd && rs[k+ticks] == '`' {
				ticks++
			}
			rest := string(rs[k+ticks : lineEnd])
			if ticks >= 3 && !strings.ContainsRune(rest, '`') {
				if !inFence {
					inFence = true
					fences = append(fences, fenceSpan{start: lineStart})
				} else {
					inFence = false
					fences[len(fences)-1].end = lineEnd
					if lineEnd < n && rs[lineEnd] == '\n' {
						fences[len(fences)-1].end = lineEnd + 1
					}
				}
				i = lineEnd + 1
				continue
			}
		}
		if !inFence {
			if lvl, title, ok := atxHeading(rs[k:lineEnd]); ok {
				headings = append(headings, heading{offset: lineStart, level: lvl, title: title})
			}
		}
		i = lineEnd + 1
	}
	if inFence && len(fences) > 0 {
		fences[len(fences)-1].end = n // 未闭合围栏延伸到文末
	}
	return headings, fences
}

// atxHeading 判断（已 trim 的）行是否 ATX 标题：#×1..6 且后跟空格/行尾
// （CommonMark 规则，防止 #hashtag / C# 注释行误判）。
func atxHeading(line []rune) (int, string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if level < len(line) && line[level] != ' ' && line[level] != '\t' {
		return 0, "", false
	}
	return level, strings.TrimSpace(string(line[level:])), true
}

// breadcrumbPaths 六层标题栈（压入 level-N 弹出所有 ≥N）→ 每个标题的面包屑
// 路径（"H1 > H2 > H3"，标题文本与总长截断见 headingTitleMax/sectionPathMax）。
func breadcrumbPaths(headings []heading) []string {
	type node struct {
		level int
		title string
	}
	var stack []node
	out := make([]string, len(headings))
	for k, h := range headings {
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, node{h.level, h.title})
		parts := make([]string, 0, len(stack))
		for _, node := range stack {
			parts = append(parts, truncateRunes(node.title, headingTitleMax))
		}
		out[k] = truncatePath(strings.Join(parts, " > "))
	}
	return out
}

// truncateRunes rune 截断（超长补省略号）。
func truncateRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max-1]) + "…"
}

// truncatePath 面包屑总长截断：超长自最老一级开始丢弃（深层标题更具区分度）。
func truncatePath(path string) string {
	for len([]rune(path)) > sectionPathMax {
		parts := strings.SplitN(path, " > ", 2)
		if len(parts) < 2 {
			return truncateRunes(path, sectionPathMax)
		}
		path = parts[1]
	}
	return path
}
