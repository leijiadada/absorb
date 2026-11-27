package document_test

// miss_autopsy_test.go —— 48 题 miss 逐题归因（只读诊断，2026-11-18）：
//
// F3 系列关闭后，剩余 miss（plain 基线 57%/57%）的瓶颈构成未知。本测试复用
// ?poppler-eval=1 plain 镜像（F3c 定稿形态）与冻结 48 题，逐题采集四路排名
// 与锚点块事实，按 design/5.md「Miss Autopsy 预注册」决策树归类：
//
//	P1 候选池缺失·跨块切断（E=0，无单块含完整锚点）
//	P2 候选池缺失·双路盲区（L=0 ∧ V=0，两路 top50 都没有锚点块）
//	P3 词汇不匹配（L=0 ∧ V>0，词法盲向量通——查询改写主战场）
//	P4 嵌入稀释（L>0 ∧ V=0，词法通向量盲）
//	P5 融合截断（L>0 ∧ V>0，三类之外单列——topN/融合权重问题）
//
// 只读：UpsertDocument 取既有 docID，不写内容、不建索引；镜像或向量缺失
// 时 Skip（先跑 TestSynonymRetrievalMarksAB 建镜像）。云模式未配置 Skip
// （归因口径 = 生产云混合形态）。
//
// 采集字段与决策规则见 design/5.md 预注册节（先写后跑）。
//
// 运行：
//
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_EMB_API_KEY="<siliconflow key>"
//	$env:ABSORB_EMB_MODEL="Qwen/Qwen3-Embedding-4B"; $env:ABSORB_RR_MODEL="Qwen/Qwen3-Reranker-4B"
//	go test ./internal/document/ -run TestMissAutopsy -count=1 -v

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"absorb-agent/internal/document"
	"absorb-agent/internal/retrieval"
	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
	searchdoc "absorb-agent/tool/search_document"
)

func TestMissAutopsy(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("归因口径 = 生产云混合形态；设置 ABSORB_CLOUD_EMBED=1 后运行")
	}
	embKey := os.Getenv("ABSORB_EMB_API_KEY")
	if embKey == "" {
		t.Fatal("cloud 模式需要 ABSORB_EMB_API_KEY")
	}
	embModel := os.Getenv("ABSORB_EMB_MODEL")
	if embModel == "" {
		embModel = "Qwen/Qwen3-Embedding-4B"
	}
	rrModel := os.Getenv("ABSORB_RR_MODEL")
	if rrModel == "" {
		rrModel = "Qwen/Qwen3-Reranker-4B"
	}

	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	const (
		bookURL = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"
		app     = "absorb-reader"
		user    = "absorb-user"
	)
	mdoc, err := st.UpsertDocument(ctx, app, user, bookURL+popplerMirrorSuffix, "AgenticSE Book CN (poppler)")
	if err != nil {
		t.Fatalf("upsert mirror: %v", err)
	}
	state, _ := st.LoadDocumentIndexState(ctx, app, user, mdoc.ID)
	if state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Skipf("plain 镜像未就绪（先跑 TestSynonymRetrievalMarksAB 建镜像）: %+v", state)
	}

	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	reranker := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)

	vhits, vrows, err := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{mdoc.ID})
	if err != nil || len(vrows) == 0 {
		t.Skipf("镜像向量缺失 (model=%s, err=%v)——先跑 TestSynonymRetrievalMarksAB", emb.Model(), err)
	}
	chunks, err := st.LoadDocumentChunks(ctx, app, user, mdoc.ID)
	if err != nil || len(chunks) == 0 {
		t.Skipf("镜像块缺失 (err=%v)", err)
	}
	t.Logf("镜像：%d 块 / %d 向量（model=%s）｜生产管线：词法50+向量50→RRF(60)→精排%d→去重10",
		len(chunks), len(vrows), emb.Model(), rrTopN)

	a := document.SharedAnalyzer()

	type rowOut struct {
		name           string
		verdict        string
		e              int
		l, lfull       int
		v, vfull       int
		f, rr, fin     int
		cos            float64
		applied        bool
		tokHit, tokAll int
		tokMissing     string
	}
	rows := make([]rowOut, 0, len(synonymCases))

	keyOf := func(h storage.DocumentChunkHit) string {
		return h.DocumentID + "#" + strconv.Itoa(h.ChunkIndex)
	}
	rankOfKeys := func(hits []storage.DocumentChunkHit, keys map[string]struct{}) int {
		for i, h := range hits {
			if _, ok := keys[keyOf(h)]; ok {
				return i + 1
			}
		}
		return 0
	}

	for _, c := range synonymCases {
		row := rowOut{name: c.name}

		// E：包含完整锚点的块（滑窗切块下 0/1/多皆可能）
		keys := make(map[string]struct{}, 2)
		firstIdx := -1
		for i, ch := range chunks {
			if strings.Contains(ch.Content, c.anchor) {
				keys[mdoc.ID+"#"+strconv.Itoa(ch.ChunkIndex)] = struct{}{}
				if firstIdx < 0 {
					firstIdx = i
				}
			}
		}
		row.e = len(keys)

		// 词法路：生产池（50）+ 全量（400 > 全部块数）
		qexpr := a.QueryExpression(c.query)
		lex50, err := st.SearchDocumentChunks(ctx, app, user, qexpr, []string{mdoc.ID}, 50)
		if err != nil {
			t.Fatalf("lex search %q: %v", c.query, err)
		}
		lexFull, err := st.SearchDocumentChunks(ctx, app, user, qexpr, []string{mdoc.ID}, 400)
		if err != nil {
			t.Fatalf("lex full search %q: %v", c.query, err)
		}
		row.l = rankOfKeys(lex50, keys)
		row.lfull = rankOfKeys(lexFull, keys)

		// 向量路：生产池（50）+ 全量
		qv, err := emb.EmbedQuery(ctx, c.query)
		if err != nil {
			t.Fatalf("embed %q: %v", c.query, err)
		}
		vec50 := retrieval.TopKByCosine(vhits, vrows, qv, 50)
		vecAll := retrieval.TopKByCosine(vhits, vrows, qv, len(vrows))
		row.v = rankOfKeys(vec50, keys)
		row.vfull = rankOfKeys(vecAll, keys)

		// 锚点块余弦分（首个含锚点块）。vhits/vrows 同源同序，ChunkIndex 在 hit 侧。
		if firstIdx >= 0 {
			want := chunks[firstIdx].ChunkIndex
			for i := range vrows {
				if vhits[i].ChunkIndex == want {
					row.cos = cosineOf(qv, vrows[i].Vector)
					break
				}
			}
		}

		// 融合 → 精排 → 生产判定（top10 内容含锚点）
		fused := retrieval.FuseRRF(lex50, vec50, 50)
		row.f = rankOfKeys(fused, keys)
		rr, applied := retrieval.RerankCandidates(ctx, fused, c.query, reranker, rrTopN)
		row.applied = applied
		row.rr = rankOfKeys(rr, keys)
		final, _ := searchdoc.DeduplicateHits(rr, 10)
		for i, h := range final {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				row.fin = i + 1
				break
			}
		}

		// 诊断：锚点块 search_text 对查询 token 的覆盖（宽松子串口径）
		if firstIdx >= 0 {
			stText := chunks[firstIdx].SearchText
			for _, tok := range parseQueryTokens(qexpr) {
				row.tokAll++
				if strings.Contains(stText, tok) {
					row.tokHit++
				} else {
					row.tokMissing += tok + " "
				}
			}
		}

		// 预注册决策树（按序匹配、命中即停）
		switch {
		case row.fin > 0:
			row.verdict = "HIT"
		case row.e == 0:
			row.verdict = "P1跨块"
		case row.l == 0 && row.v == 0:
			row.verdict = "P2双路盲区"
		case row.l == 0:
			row.verdict = "P3词汇不匹配"
		case row.v == 0:
			row.verdict = "P4嵌入稀释"
		default:
			row.verdict = "P5融合截断"
		}
		rows = append(rows, row)
	}

	// 明细表（48 行）+ 汇总
	t.Logf("%-24s %-14s %2s %4s %4s %4s %4s %5s %3s %3s %3s %s",
		"题目", "判定", "E", "L", "Lf", "V", "Vf", "cos", "F", "RR", "fin", "块内缺词")
	rrFallback := 0
	counts := map[string]int{}
	synMiss, ctlMiss, hitN := 0, 0, 0
	p2Near := 0 // P2 中 Vfull≤80 的改写可及边缘题
	for _, r := range rows {
		rrmark := ""
		if !r.applied {
			rrmark = "*"
			rrFallback++
		}
		t.Logf("%-24s %-13s%s %2d %4d %4d %4d %4d %5.3f %3d %3d %3d [%d/%d %s]",
			r.name, r.verdict, rrmark, r.e, r.l, r.lfull, r.v, r.vfull, r.cos,
			r.f, r.rr, r.fin, r.tokHit, r.tokAll, strings.TrimSpace(r.tokMissing))
		counts[r.verdict]++
		isSyn := !strings.HasPrefix(r.name, "对照")
		if r.verdict == "HIT" {
			hitN++
			continue
		}
		if isSyn {
			synMiss++
		} else {
			ctlMiss++
		}
		if r.verdict == "P2双路盲区" && r.vfull > 0 && r.vfull <= 80 {
			p2Near++
		}
	}

	synN := len(synonymCases) - 4
	t.Logf("")
	t.Logf("==== Miss Autopsy（同义组 %d 题，云混合单轮，plain 镜像 %d 块）====", synN, len(chunks))
	t.Logf("命中 %d ｜ miss %d（同义 %d + 对照 %d）", hitN, synMiss+ctlMiss, synMiss, ctlMiss)
	t.Logf("同义组归因：P1跨块=%d  P2双路盲区=%d（其中 Vfull≤80 边缘 %d）  P3词汇不匹配=%d  P4嵌入稀释=%d  P5融合截断=%d",
		counts["P1跨块"], counts["P2双路盲区"], p2Near, counts["P3词汇不匹配"], counts["P4嵌入稀释"], counts["P5融合截断"])
	if ctlMiss > 0 {
		t.Logf("对照组 miss %d——评测集可达性异常，单独排查", ctlMiss)
	}
	if rrFallback > 0 {
		t.Logf("警告：%d 条精排失败降级融合序（RR 列带 *），相关题归因按 P5 谨慎解读", rrFallback)
	}
	t.Logf("决策规则（预注册）：P3 为最大桶且 ≥1/3 → 下一个杠杆 = 查询改写；否则按主导桶定向")
}

// parseQueryTokens 从 QueryExpression 产出（"t1" OR "t2" ...）还原 token 列表。
func parseQueryTokens(expr string) []string {
	var out []string
	for _, part := range strings.Split(expr, " OR ") {
		t := strings.TrimSpace(part)
		t = strings.Trim(t, `"`)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// cosineOf 查询向量与块向量的余弦相似度（TopKByCosine 不回填 Score，独立实现）。
func cosineOf(q, v []float32) float64 {
	if len(q) != len(v) || len(q) == 0 {
		return 0
	}
	var dot, nq, nv float64
	for i := range q {
		dot += float64(q[i]) * float64(v[i])
		nq += float64(q[i]) * float64(q[i])
		nv += float64(v[i]) * float64(v[i])
	}
	if nq == 0 || nv == 0 {
		return 0
	}
	return dot / (math.Sqrt(nq) * math.Sqrt(nv))
}
