package searchdoc

// DiversifyHits（M2 融合层）：去重、单文档轮转分配（防长文垄断）、总量控制。

import (
	"strings"
	"testing"

	"absorb-agent/internal/store/storage"
)

func hit(doc, content string, score float64) storage.DocumentChunkHit {
	return storage.DocumentChunkHit{DocumentID: doc, Title: "T-" + doc, Content: content, Score: score}
}

// TestDiversifyHitsPerDocCap 单文档垄断被压制：大 PDF 占满 raw 前 10 名时，
// 最终结果单文档不超过 perDoc 上限。
func TestDiversifyHitsPerDocCap(t *testing.T) {
	var raw []storage.DocumentChunkHit
	for i := 0; i < 10; i++ {
		raw = append(raw, hit("big", strings.Repeat("大文档片段", 10)+string(rune('a'+i)), -float64(i+1)))
	}
	raw = append(raw, hit("small", "小文档的独家证据", -20))
	final, docs := DiversifyHits(raw, 3, 8)
	if docs != 2 {
		t.Fatalf("docs covered = %d, want 2", docs)
	}
	big := 0
	for _, h := range final {
		if h.DocumentID == "big" {
			big++
		}
	}
	if big != 3 {
		t.Fatalf("big doc hits = %d, want capped at 3", big)
	}
	if len(final) != 4 {
		t.Fatalf("final = %d, want 4", len(final))
	}
	// 小文档的独家证据必须在场（轮转分配保住了它的曝光）
	found := false
	for _, h := range final {
		if strings.Contains(h.Content, "独家证据") {
			found = true
		}
	}
	if !found {
		t.Fatal("small doc evidence lost — anti-monopoly broken")
	}
}

// TestDiversifyHitsDedup 归一化重复只保留 bm25 最优一条。
func TestDiversifyHitsDedup(t *testing.T) {
	raw := []storage.DocumentChunkHit{
		hit("d1", "完全 相同 的 内容", -1),
		hit("d1", "完全相同的内容", -2),       // 归一化后与前条相同
		hit("d1", "完全  相同\t的\n内容", -3), // 空白差异
		hit("d2", "不同的内容", -4),
	}
	final, docs := DiversifyHits(raw, 3, 8)
	if len(final) != 2 || docs != 2 {
		t.Fatalf("dedup failed: len=%d docs=%d, want 2/2", len(final), docs)
	}
	if final[0].Score != -1 {
		t.Fatalf("dedup must keep best bm25, got score=%v", final[0].Score)
	}
}

// TestDiversifyHitsTotalCap 总量控制与顺序保持（bm25 相对顺序）。
func TestDiversifyHitsTotalCap(t *testing.T) {
	var raw []storage.DocumentChunkHit
	for i := 0; i < 20; i++ {
		raw = append(raw, hit("d"+string(rune('a'+i%4)), "片段"+string(rune('A'+i)), -float64(i+1)))
	}
	final, docs := DiversifyHits(raw, 2, 6)
	if len(final) != 6 || docs != 4 {
		t.Fatalf("len=%d docs=%d, want 6/4", len(final), docs)
	}
	// bm25 升序输出：数值递减（越小越相关在前），保持 raw 相对顺序
	for i := 1; i < len(final); i++ {
		if final[i].Score > final[i-1].Score {
			t.Fatalf("bm25 order broken at %d: %v > %v", i, final[i-1].Score, final[i].Score)
		}
	}
}

// TestDiversifyHitsEmpty 空入参安全。
func TestDiversifyHitsEmpty(t *testing.T) {
	final, docs := DiversifyHits(nil, 3, 8)
	if final != nil || docs != 0 {
		t.Fatalf("empty mismatch: (%v, %d)", final, docs)
	}
}
