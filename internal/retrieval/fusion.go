// RRF（Reciprocal Rank Fusion，doc1 §6.4）：双路检索结果融合。
// 不比较 bm25 与余弦的原始分（语义不可通约），只按名次计分：
//
//	score(chunk) = Σ 1/(k + rank_i)，k=60
//
// 同一 chunk 双路命中 → 分数叠加，天然奖励「词法+语义都认为相关」的片段。
package retrieval

import (
	"math"
	"strconv"

	"absorb-agent/internal/store/storage"
)

// RRFK 名次平滑常数（业界默认 60）：头部名次差距被压平，避免单路 top1 独裁。
const RRFK = 60

// FuseRRF 融合两路命中（各按自身相关度降序传入），返回按 RRF 分降序的
// 前 limit 条。同 chunk（documentID+chunkIndex 定位）双路命中分数叠加，
// 保留内容 richer 的一份（bm25 路的 hit 已带标题，向量路经 JOIN 同样带）。
func FuseRRF(lexical, vector []storage.DocumentChunkHit, limit int) []storage.DocumentChunkHit {
	type entry struct {
		hit   storage.DocumentChunkHit
		score float64
	}
	index := make(map[string]int, len(lexical)+len(vector))
	fused := make([]entry, 0, len(lexical)+len(vector))
	add := func(list []storage.DocumentChunkHit) {
		for rank, h := range list {
			key := h.DocumentID + "#" + strconv.Itoa(h.ChunkIndex)
			if pos, ok := index[key]; ok {
				fused[pos].score += 1.0 / float64(RRFK+rank+1)
				continue
			}
			index[key] = len(fused)
			fused = append(fused, entry{hit: h, score: 1.0 / float64(RRFK+rank+1)})
		}
	}
	add(lexical)
	add(vector)

	// 插入排序：候选 ≤ 2×limit 量级，无需引入 sort 依赖开销
	for i := 1; i < len(fused); i++ {
		for j := i; j > 0 && fused[j].score > fused[j-1].score; j-- {
			fused[j], fused[j-1] = fused[j-1], fused[j]
		}
	}
	if limit > 0 && len(fused) > limit {
		fused = fused[:limit]
	}
	out := make([]storage.DocumentChunkHit, 0, len(fused))
	for _, e := range fused {
		out = append(out, e.hit)
	}
	return out
}

// TopKByCosine 语义召回：查询向量与全部 chunk 向量算余弦相似度，取 top limit。
// rows 与 hits 同序同源（LoadVectorHits 保证）；dims 不匹配或空向量的行跳过。
func TopKByCosine(hits []storage.DocumentChunkHit, rows []storage.ChunkVectorRow, query []float32, limit int) []storage.DocumentChunkHit {
	if limit <= 0 {
		return nil
	}
	type scored struct {
		hit   storage.DocumentChunkHit
		score float64
	}
	out := make([]scored, 0, len(rows))
	for i := range rows {
		v := rows[i].Vector
		if len(v) != len(query) || len(v) == 0 {
			continue
		}
		var dot, nq, nv float64
		for j := range v {
			dot += float64(query[j]) * float64(v[j])
			nq += float64(query[j]) * float64(query[j])
			nv += float64(v[j]) * float64(v[j])
		}
		if nq == 0 || nv == 0 {
			continue
		}
		out = append(out, scored{hit: hits[i], score: dot / (math.Sqrt(nq) * math.Sqrt(nv))})
	}
	// 部分选择：只排前 limit（候选规模小，简单选择排序足够）
	n := limit
	if n > len(out) {
		n = len(out)
	}
	for i := 0; i < n; i++ {
		best := i
		for j := i + 1; j < len(out); j++ {
			if out[j].score > out[best].score {
				best = j
			}
		}
		out[i], out[best] = out[best], out[i]
	}
	res := make([]storage.DocumentChunkHit, 0, n)
	for i := 0; i < n; i++ {
		res = append(res, out[i].hit)
	}
	return res
}
