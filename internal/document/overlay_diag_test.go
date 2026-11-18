package document_test

// 归因诊断（Phase D1 → F3a 修订，2026-11-18）：poppler 纯文本 vs marks 元数据
// 镜像逐题对比，对退步题 dump 各路排名与候选构成。D1 原本对比「注入标题行的
// 叠加语料」，证伪连坐、锁定真因为标题行注入扰动块边界（M4）后，F3a 把结构
// 改进元数据层——正文与 plain 完全同一份，差异只剩 SectionPath（FTS 加权 +
// 向量化前置面包屑），本测试随之改为对比 marks 元数据镜像，继续提供逐题
// 排名/拥挤度归因。marks 经 extract.MarksForDiag 手工构造（不修改生产门控）。
// 云模式门控（ABSORB_CLOUD_EMBED=1）；复用开发库副本与冻结 48 题。
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"absorb-agent/internal/document"
	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/retrieval"
	"absorb-agent/internal/store/storage"
)

const overlayDiagMirrorSuffix = "?overlay-diag-eval=1"

// diagRanks 单题四路排名（0 = 未进该路 top-50 / 未命中）。
type diagRanks struct {
	lexRank   int
	vecRank   int
	fuseRank  int // 融合后、精排前
	finalRank int // 精排重排后（去重前列表中的位置）
}

// diagRunPipeline 单文档单题跑完整混合管线，返回排名与两份候选列表。
func diagRunPipeline(ctx context.Context, st storage.Storage, qexpr, query, anchor, docID string,
	qv []float32, vhits []storage.DocumentChunkHit, vrows []storage.ChunkVectorRow,
	reranker retrieval.Reranker) (diagRanks, []storage.DocumentChunkHit, []storage.DocumentChunkHit) {

	lex, err := st.SearchDocumentChunks(ctx, "absorb-reader", "absorb-user", qexpr, []string{docID}, 50)
	if err != nil {
		return diagRanks{}, nil, nil
	}
	vec := retrieval.TopKByCosine(vhits, vrows, qv, 50)
	fused := retrieval.FuseRRF(lex, vec, 50)
	final := fused
	if reranker != nil {
		final, _ = retrieval.RerankCandidates(ctx, fused, query, reranker, 16)
	}
	r := diagRanks{
		lexRank:  diagAnchorRank(lex, anchor),
		vecRank:  diagAnchorRank(vec, anchor),
		fuseRank: diagAnchorRank(fused, anchor),
	}
	r.finalRank = diagAnchorRank(final, anchor)
	return r, lex, final
}

func diagAnchorRank(hits []storage.DocumentChunkHit, anchor string) int {
	for i, h := range hits {
		if strings.Contains(h.Content, anchor) {
			return i + 1
		}
	}
	return 0
}

// TestOverlayAttributionDiag 主入口：双镜像逐题对比 + 退步题归因 dump
// （F3a：marks 元数据镜像，正文与 plain 同一份，chunk 边界逐位一致）。
func TestOverlayAttributionDiag(t *testing.T) {
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("需要 ABSORB_CLOUD_EMBED=1（云嵌入 + 云精排）")
	}
	bin := popplerBin(t)
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	const bookURL = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"
	const (
		app  = "absorb-reader"
		user = "absorb-user"
	)
	if dc, err := st.LoadDocumentContent(ctx, app, user, bookURL); err != nil || dc == nil {
		t.Skipf("副本中无目标文档正文 (err=%v)", err)
	}

	// 云装配（与冻结评测同款）
	embKey := os.Getenv("ABSORB_EMB_API_KEY")
	if embKey == "" {
		t.Fatal("需要 ABSORB_EMB_API_KEY")
	}
	embModel := os.Getenv("ABSORB_EMB_MODEL")
	if embModel == "" {
		embModel = "Qwen/Qwen3-Embedding-4B"
	}
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	retrieval.SetSharedEmbedder(emb)
	rrModel := os.Getenv("ABSORB_RR_MODEL")
	if rrModel == "" {
		rrModel = "Qwen/Qwen3-Reranker-4B"
	}
	reranker := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)

	// 语料：poppler 纯文本一份，两镜像共享正文（plain 无结构 / marks 有结构
	// 元数据——chunk 边界逐位一致，差异只剩 SectionPath）
	proxyFn := func() string { return os.Getenv("ABSORB_PROXY_URL") }
	data, err := fetchBytesForTest(ctx, proxyFn, bookURL, 50<<20)
	if err != nil {
		t.Skipf("下载语料失败: %v", err)
	}
	popplerText, err := extract.PopplerText(context.Background(), bin, data)
	if err != nil {
		t.Fatalf("PopplerText: %v", err)
	}
	marks, injected := extract.MarksForDiag(data, popplerText)
	t.Logf("marks 镜像：%d 字，命中 %d 个标题（正文与 plain 同一份）",
		len([]rune(popplerText)), injected)
	if injected == 0 {
		t.Fatal("marks 命中 0 个——归因无意义")
	}
	marksJSON, err := json.Marshal(marks)
	if err != nil {
		t.Fatalf("marshal marks: %v", err)
	}

	svc := document.NewService(st, nil, app, user).WithEmbedder(emb)
	buildMirror := func(text, headingMarks, suffix, title string) *storage.Document {
		mirrorURL := bookURL + suffix
		mdc := &storage.DocumentContent{
			URL: mirrorURL, ContentText: text,
			CharCount: len([]rune(text)), ContentType: "application/pdf",
			HeadingMarks: headingMarks,
		}
		mdc.ContentHash = sha256Sum(text)
		mdc.ByteSize = int64(len(text))
		mdoc, err := st.UpsertDocument(ctx, app, user, mirrorURL, title)
		if err != nil {
			t.Fatalf("upsert %s: %v", title, err)
		}
		if err := st.SaveDocumentContent(ctx, app, user, mdc); err != nil {
			t.Fatalf("save %s: %v", title, err)
		}
		svc.BuildIndex(ctx, mdoc.ID, mdc)
		if state, _ := st.LoadDocumentIndexState(ctx, app, user, mdoc.ID); state == nil || state.IndexStatus != storage.IndexStatusReady {
			t.Fatalf("%s index not ready", title)
		}
		return mdoc
	}
	mdocPlain := buildMirror(popplerText, "", popplerMirrorSuffix, "AgenticSE Book CN (poppler)")
	mdocOv := buildMirror(popplerText, string(marksJSON), overlayDiagMirrorSuffix, "AgenticSE Book CN (marks-diag)")
	// 块边界等价不变式：正文同一份 → chunk 数必须一致
	if stateP, _ := st.LoadDocumentIndexState(ctx, app, user, mdocPlain.ID); stateP != nil {
		if stateO, _ := st.LoadDocumentIndexState(ctx, app, user, mdocOv.ID); stateO != nil {
			if stateP.ChunkCount != stateO.ChunkCount {
				t.Fatalf("chunk 数漂移：plain %d vs marks %d（正文一字节不动的等价不变式被破坏）",
					stateP.ChunkCount, stateO.ChunkCount)
			}
		}
	}

	// 两文档向量一次加载（行不随题变化）
	vhP, vrP, err := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{mdocPlain.ID})
	if err != nil || len(vrP) == 0 {
		t.Fatalf("plain 向量未入库: %v", err)
	}
	vhO, vrO, err := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{mdocOv.ID})
	if err != nil || len(vrO) == 0 {
		t.Fatalf("overlay 向量未入库: %v", err)
	}

	a := document.SharedAnalyzer()
	type verdict struct {
		name     string
		plainR   diagRanks
		ovR      diagRanks
		anchorLn int
		path     string
		crowd    int // 精排输入 top-16 中与锚点同 section_path 的块数
		lexCrowd int // 词法 top-50 中同 path 块数
	}
	var regressed, improved []verdict
	for _, c := range synonymCases {
		if strings.HasPrefix(c.name, "对照") {
			continue
		}
		qexpr := a.QueryExpression(c.query)
		qv, qerr := emb.EmbedQuery(ctx, c.query)
		if qerr != nil {
			t.Fatalf("embed %q: %v", c.query, qerr)
		}
		rp, _, _ := diagRunPipeline(ctx, st, qexpr, c.query, c.anchor, mdocPlain.ID, qv, vhP, vrP, reranker)
		ro, lexO, finalO := diagRunPipeline(ctx, st, qexpr, c.query, c.anchor, mdocOv.ID, qv, vhO, vrO, reranker)

		// 锚点块信息（overlay 语料）：取词法命中里的锚点块
		v := verdict{name: c.name, plainR: rp, ovR: ro}
		for _, h := range lexO {
			if strings.Contains(h.Content, c.anchor) {
				v.anchorLn = len([]rune(h.Content))
				v.path = h.SectionPath
				break
			}
		}
		// 拥挤度：精排输入 top-16（finalO 前 16 位 = 重排输入）与词法 top-50
		limit := 16
		if len(finalO) < limit {
			limit = len(finalO)
		}
		for _, h := range finalO[:limit] {
			if v.path != "" && h.SectionPath == v.path {
				v.crowd++
			}
		}
		for _, h := range lexO {
			if v.path != "" && h.SectionPath == v.path {
				v.lexCrowd++
			}
		}

		regressedQ := (rp.finalRank > 0 && (ro.finalRank == 0 || ro.finalRank > rp.finalRank))
		improvedQ := (ro.finalRank > 0 && (rp.finalRank == 0 || ro.finalRank < rp.finalRank))
		if regressedQ {
			regressed = append(regressed, v)
			t.Logf("[退步] %-24s plain:lex%d/vec%d/fuse%d/final%d → overlay:lex%d/vec%d/fuse%d/final%d | 锚点块=%d字 path=%q | top16同path=%d lex50同path=%d",
				c.name, rp.lexRank, rp.vecRank, rp.fuseRank, rp.finalRank,
				ro.lexRank, ro.vecRank, ro.fuseRank, ro.finalRank,
				v.anchorLn, truncStr(v.path, 30), v.crowd, v.lexCrowd)
		} else if improvedQ {
			improved = append(improved, v)
			t.Logf("[改善] %-24s plain final%d → overlay final%d", c.name, rp.finalRank, ro.finalRank)
		}
	}

	// 机制判定汇总
	m1, m2 := 0, 0
	for _, v := range regressed {
		switch {
		case v.ovR.vecRank == 0 && v.plainR.vecRank > 0:
			m2++ // 向量路丢失（块切分削弱语义）
		case v.ovR.fuseRank > 0 && v.ovR.finalRank == 0:
			m1++ // 融合池可见但精排后消失（候选拥挤挤出）
		}
	}
	t.Logf("==== 归因汇总：退步 %d 题，改善 %d 题；M1 连坐候选 %d，M2 向量丢失 %d ====",
		len(regressed), len(improved), m1, m2)
}

// truncStr rune 截断（dump 展示用）。
func truncStr(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// sha256Sum 文本哈希（hex）。
func sha256Sum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
