// chunker（M1）：document_contents.content_text → 检索片段。
// 设计约束（doc2 §3.2）：偏移量是唯一可靠定位手段（提取器无页码/章节），
// 因此 Chunk 的 [StartOffset,EndOffset) 必须满足 text[o.Start:o.End] == o.Content
// （rune 偏移，与 CharCount 同口径）；section_path 允许为空，不强行造章节树。
package document

// ChunkerVersion 切块规则版本：窗口/边界策略变化时 +1，触发全量索引重建。
// v2（2026-11-15）：目标块 800→400——bge 系 embedding 训练上下文 512 token
// （中文 ≈1 token/字），块超限时尾部内容不进向量（llama-server 直接拒绝超长
// 输入，客户端截断兜底），语义检索对块尾部信息天然失明；块尺寸对齐模型上下文。
const ChunkerVersion = 2

const (
	chunkTargetRunes  = 400 // 目标块长（rune）：≤ bge 512 token 上下文，向量完整覆盖块内全部信息
	chunkOverlapRunes = 100 // 相邻块重叠：跨块边界的句子不因切割而检索丢失
	minChunkRunes     = 80  // 尾块合并阈值：避免产出只有一两句的碎块
	breakLookback     = 200 // 断句点向前搜索的最大距离（半个窗口内必有自然边界）
)

// textChunk 切块产物：原文 + rune 偏移。
type textChunk struct {
	Content     string
	StartOffset int
	EndOffset   int
}

// breakRunes 优先级从高到低的自然断点：空行（段落）> 换行 > 句末标点。
var breakRunes = map[rune]int{'\n': 2, '。': 1, '！': 1, '？': 1, '!': 1, '?': 1, '；': 1, ';': 1}

// ChunkText 把正文切成带偏移的检索片段：优先自然边界（段落/句子），
// 无边界时硬切；相邻块重叠 chunkOverlapRunes；末尾碎块并入前块。
func ChunkText(text string) []textChunk {
	rs := []rune(text)
	n := len(rs)
	if n == 0 {
		return nil
	}
	var out []textChunk
	start := 0
	for start < n {
		end := start + chunkTargetRunes
		if end >= n {
			out = append(out, textChunk{string(rs[start:]), start, n})
			break
		}
		brk := findBreak(rs, start, end)
		if brk <= start {
			brk = end
		}
		out = append(out, textChunk{string(rs[start:brk]), start, brk})
		// 重叠回退：但不小于 start+1，保证单调前进
		next := brk - chunkOverlapRunes
		if next <= start {
			next = start + 1
		}
		start = next
	}
	// 末尾碎块并入前块（前块直接延伸到文末）
	if len(out) >= 2 {
		last := out[len(out)-1]
		if last.EndOffset-last.StartOffset < minChunkRunes {
			prev := &out[len(out)-2]
			prev.EndOffset = last.EndOffset
			prev.Content = string(rs[prev.StartOffset:prev.EndOffset])
			out = out[:len(out)-1]
		}
	}
	return out
}

// findBreak 在 [end-breakLookback, end] 内找最靠后的自然断点（段落 > 句末标点），
// 返回断点后的位置（含断点字符本身）；找不到返回 -1。
func findBreak(rs []rune, start, end int) int {
	lo := end - breakLookback
	if lo < start {
		lo = start
	}
	best := -1
	bestPrio := 0
	for i := end - 1; i >= lo; i-- {
		prio, ok := breakRunes[rs[i]]
		if !ok || prio <= bestPrio {
			continue
		}
		// 断点字符算进当前块
		best = i + 1
		bestPrio = prio
		if prio == 2 { // 空行已是最高优先级，直接采用
			return best
		}
	}
	return best
}
