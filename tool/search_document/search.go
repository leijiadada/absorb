// 结果融合（M2，doc2 §6.2）：跨文档检索结果不能被单篇长文垄断。
// 管线：FTS5 bm25 召回 rawRecallLimit → 归一化去重 → 单文档轮转分配
// （bm25 优先级保留，单文档不超过 perDocLimit）→ 总量 finalLimit。
package searchdoc

import (
	"strings"

	"absorb-agent/internal/store/storage"
)

// 融合参数（doc2 §6.2 初始配置，待真实语料评测调整）。
const (
	RawRecallLimit = 50 // SQL 召回上限：给融合层留足选择空间
	PerDocLimit    = 3  // 最终单文档片段上限
	FinalLimit     = 8  // 最终证据片段总量（doc2 建议 5~10）
)

// DiversifyHits 文档多样性融合：
//  1. 去重——归一化（空白折叠）后内容相同的片段只保留 bm25 最优的一条；
//  2. 单文档轮转分配——按 bm25 升序扫描，每份文档最多 perDoc 条，
//     防止 20 万字大 PDF 刷屏、短文永远无曝光；
//  3. 总量控制——凑满 total 条或扫描完为止。
//
// 返回最终片段（保持 bm25 相对顺序）与覆盖的文档数。入参假定已按 bm25 升序。
func DiversifyHits(raw []storage.DocumentChunkHit, perDoc, total int) ([]storage.DocumentChunkHit, int) {
	if len(raw) == 0 {
		return nil, 0
	}
	if perDoc <= 0 {
		perDoc = PerDocLimit
	}
	if total <= 0 {
		total = FinalLimit
	}
	seen := make(map[string]struct{}, len(raw))
	perDocCount := make(map[string]int, 8)
	out := make([]storage.DocumentChunkHit, 0, total)
	for i := range raw {
		h := raw[i]
		key := normalizeForDedup(h.Content)
		if _, dup := seen[key]; dup {
			continue
		}
		if perDocCount[h.DocumentID] >= perDoc {
			continue
		}
		seen[key] = struct{}{}
		perDocCount[h.DocumentID]++
		out = append(out, h)
		if len(out) >= total {
			break
		}
	}
	return out, len(perDocCount)
}

// normalizeForDedup 去重键：折叠全部空白后取内容——chunk 相邻块有 120 rune
// 重叠但整块内容不同，归一化相同即视为同一片段（重复召回）。
func normalizeForDedup(content string) string {
	return strings.Join(strings.Fields(content), "")
}

// DeduplicateHits 单文档范围的结果裁剪（M3 修正）：只做内容去重 + 总量控制，
// 不施加每文档配额——范围已限定（通常为当前阅读的单篇文档），per-doc 轮转
// 只会把 top_k 硬性压到 perDoc（3 < 5/10），Recall@K 名不副实。跨文档范围
// 仍走 DiversifyHits（防长文垄断）。返回最终片段与覆盖文档数，入参假定已排序。
func DeduplicateHits(raw []storage.DocumentChunkHit, total int) ([]storage.DocumentChunkHit, int) {
	if len(raw) == 0 {
		return nil, 0
	}
	if total <= 0 {
		total = FinalLimit
	}
	seen := make(map[string]struct{}, len(raw))
	docs := make(map[string]struct{}, 4)
	out := make([]storage.DocumentChunkHit, 0, total)
	for i := range raw {
		h := raw[i]
		key := normalizeForDedup(h.Content)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		docs[h.DocumentID] = struct{}{}
		out = append(out, h)
		if len(out) >= total {
			break
		}
	}
	return out, len(docs)
}
