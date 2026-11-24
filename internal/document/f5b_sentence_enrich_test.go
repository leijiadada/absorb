package document_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/document"
	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/retrieval"
	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
	fetch "absorb-agent/tool/fetch_page"
)

// f5b_sentence_enrich_test.go —— F5b：按句子边界 + 去重的邻居富化（预注册见 design/5.md）。
//
// 与 F5（机械按 150 rune 截断）的唯一差异在富化文本构造：
//  1. 按中文句末标点切句，只取完整句——不切断语义
//  2. 任一句若与本块文本重复（双向子串匹配）则跳过——消除滑窗重叠造成的重复
//
// 复用 F5 的 f5RunSuite 配对 runner（plain vs enrich，key 判定），零生产改动。

// f5bSplitSentences 按中文句末标点切句，标点保留在句尾；空串/纯空白句丢弃。
// 用 rune 遍历避免中文索引错位。
func f5bSplitSentences(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if r == '。' || r == '！' || r == '？' || r == '；' || r == '\n' {
			end := i + len(string(r))
			seg := strings.TrimSpace(s[start:end])
			if seg != "" {
				out = append(out, seg)
			}
			start = end
		}
	}
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

// f5bOverlaps 判断句 seg 是否与本块文本重复：seg 含于 cur 或 cur 的足够长片段含于 seg。
// 用双向子串+最小长度阈值，避免把单字/短句误判为重复。
func f5bOverlaps(seg, cur string) bool {
	seg = strings.TrimSpace(seg)
	rs := []rune(seg)
	if len(rs) < 8 { // 短句（如"- 102 -"）不做重复判断，直接保留其原样语义
		return false
	}
	return strings.Contains(cur, seg)
}

// f5bPickSentences 从句子序列取约 target 字的完整句（fromEnd=true 取尾部，false 取头部），
// 跳过与本块重复的句子。返回拼接文本。
func f5bPickSentences(sents []string, cur string, target int, fromEnd bool) string {
	var picked []string
	total := 0
	iter := func(i int) {
		seg := sents[i]
		if f5bOverlaps(seg, cur) {
			return
		}
		n := len([]rune(seg))
		if total+n > target && total > 0 {
			return
		}
		if fromEnd {
			picked = append([]string{seg}, picked...)
		} else {
			picked = append(picked, seg)
		}
		total += n
	}
	if fromEnd {
		for i := len(sents) - 1; i >= 0 && total < target; i-- {
			iter(i)
		}
	} else {
		for i := 0; i < len(sents) && total < target; i++ {
			iter(i)
		}
	}
	return strings.Join(picked, "\n")
}

// f5bEnrichHits 句子边界 + 去重版富化（F5b 唯一变量）。
func f5bEnrichHits(hits []storage.DocumentChunkHit, chunkByIndex map[int]string) []storage.DocumentChunkHit {
	out := make([]storage.DocumentChunkHit, len(hits))
	for i, h := range hits {
		out[i] = h
		var b strings.Builder
		if prev, ok := chunkByIndex[h.ChunkIndex-1]; ok {
			p := f5bPickSentences(f5bSplitSentences(prev), h.Content, f5NeighborRunes, true)
			if p != "" {
				b.WriteString(p)
				b.WriteByte('\n')
			}
		}
		b.WriteString(h.Content)
		if next, ok := chunkByIndex[h.ChunkIndex+1]; ok {
			n := f5bPickSentences(f5bSplitSentences(next), h.Content, f5NeighborRunes, false)
			if n != "" {
				b.WriteByte('\n')
				b.WriteString(n)
			}
		}
		out[i].Content = b.String()
	}
	return out
}

// f5bDump 冒烟：打印锚点块的 F5b 富化文本，供人工核对（不进判定）。
func TestF5BSentenceEnrichDump(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Skipf("开发库副本不存在（%s）", dbPath)
	}
	defer st.Close()
	ctx := context.Background()
	const (app, user = "absorb-reader", "absorb-user")
	doc, _ := st.UpsertDocument(ctx, app, user, book2URL, "人月神话（文字版）")
	chunks, _ := st.LoadDocumentChunks(ctx, app, user, doc.ID)
	if len(chunks) == 0 {
		t.Skip("无 chunks")
	}
	anchor := "能够独立地许诺在生产率、可靠性或简洁性上取得数量级的提高"
	byIdx := map[int]string{}
	var cur storage.DocumentChunk
	found := false
	for _, ch := range chunks {
		byIdx[ch.ChunkIndex] = ch.Content
		if !found && strings.Contains(ch.Content, anchor) {
			cur = *ch
			found = true
		}
	}
	if !found {
		t.Skip("锚点块未找到")
	}
	enriched := f5bEnrichHits([]storage.DocumentChunkHit{{
		DocumentID: doc.ID, ChunkIndex: cur.ChunkIndex, Content: cur.Content,
	}}, byIdx)[0].Content
	t.Logf("=== F5b 富化文本（锚点块 ChunkIndex=%d, plain=%d rune → enrich=%d rune）===\n%s",
		cur.ChunkIndex, len([]rune(cur.Content)), len([]rune(enriched)), enriched)
}

// TestF5BSentenceEnrichBook2 F5b 主判定集实跑（与 F5 同 setup）。
func TestF5BSentenceEnrichBook2(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在，跳过（%s）", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F5b 口径 = 云混合")
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
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	bookPath := findRepoPathFrom("docs/pdf/人月神话-中文-目录-文字版.pdf")
	if bookPath == "" {
		t.Skip("样书不存在")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	mux := http.NewServeMux()
	mux.HandleFunc("/book.pdf", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, bookPath) })
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	ex := extract.NewPDFExtractor(fetch.NewFetchPageService(func() string { return "" },
		func() int64 { return 200 << 20 }), func() string { return "" }, func() int64 { return 200 << 20 })
	xctx, xcancel := context.WithTimeout(ctx, 5*time.Minute)
	defer xcancel()
	res, err := ex.Extract(xctx, "http://"+ln.Addr().String()+"/book.pdf")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	const (app, user = "absorb-reader", "absorb-user")
	doc, _ := st.UpsertDocument(ctx, app, user, book2URL, "人月神话（文字版）")
	sum := sha256.Sum256([]byte(res.Content))
	dc := &storage.DocumentContent{URL: book2URL, ContentText: res.Content, CharCount: len([]rune(res.Content)),
		ContentType: res.ContentType, ByteSize: int64(len(res.Content)), ContentHash: hex.EncodeToString(sum[:]),
		ExtractionStatus: extract.StatusReady, ExtractionVersion: 5, FetchedAt: time.Now()}
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	retrieval.SetSharedEmbedder(emb)
	rr := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
	svc := document.NewService(st, nil, app, user).WithEmbedder(emb)
	if err := st.SaveDocumentContent(ctx, app, user, dc); err != nil {
		t.Fatal(err)
	}
	svc.BuildIndex(ctx, doc.ID, dc)
	vhits, vrows, err := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{doc.ID})
	if err != nil || len(vrows) == 0 {
		t.Fatalf("向量缺失: %v", err)
	}
	chunks, _ := st.LoadDocumentChunks(ctx, app, user, doc.ID)
	cases := make([]f5Case, 0, len(book2Cases))
	for _, c := range book2Cases {
		cases = append(cases, f5Case{c.name, c.query, c.anchor})
	}
	f5RunSuite(t, "Book2 人月神话 [F5b 句子+去重]", st, app, user, doc.ID, cases, chunks, vhits, vrows, emb, rr, f5bEnrichHits)
}

// TestF5BSentenceEnrichBook1 F5b 泛化观测：主书 AgenticSE 44 同义题（用主文档原本书，
// 与 F5 Book1 同语料口径——副本中主文档有 ready 索引/向量，镜像文档无）。
func TestF5BSentenceEnrichBook1(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在，跳过（%s）", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F5b 口径 = 云混合")
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
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const (app, user = "absorb-reader", "absorb-user")
	mdoc, _ := st.UpsertDocument(ctx, app, user, "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf", "AgenticSE Book CN")
	state, _ := st.LoadDocumentIndexState(ctx, app, user, mdoc.ID)
	if state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Skip("主书原文档未就绪")
	}
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	rr := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
	vhits, vrows, err := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{mdoc.ID})
	if err != nil || len(vrows) == 0 {
		t.Skipf("主书向量缺失: %v", err)
	}
	chunks, _ := st.LoadDocumentChunks(ctx, app, user, mdoc.ID)
	cases := make([]f5Case, 0, len(synonymCases))
	for _, c := range synonymCases {
		cases = append(cases, f5Case{c.name, c.query, c.anchor})
	}
	f5RunSuite(t, "Book1 AgenticSE [F5b 句子+去重]", st, app, user, mdoc.ID, cases, chunks, vhits, vrows, emb, rr, f5bEnrichHits)
}
