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

// f6_enrich_struct_test.go —— F6 富化结构筛选（预注册见 design/5.md）。
// B 仅上文 / C 仅下文 / D 短邻居 / E 合成标题前缀，复用 f5RunSuite + 四策略共享
// 同一套提取/索引/向量 setup（embed 只做一次）。零生产改动，key 判定。

// f6EnrichBase 通用富化构造：控制是否用 prev/next、邻居预算、是否加标题前缀。
func f6EnrichBase(hits []storage.DocumentChunkHit, chunkByIndex map[int]string,
	usePrev, useNext bool, budget int, withTitle bool) []storage.DocumentChunkHit {
	out := make([]storage.DocumentChunkHit, len(hits))
	for i, h := range hits {
		out[i] = h
		var b strings.Builder
		if withTitle {
			if title := f6FirstLineTitle(h.Content); title != "" {
				b.WriteString(title)
				b.WriteByte('\n')
			}
		}
		if usePrev {
			if prev, ok := chunkByIndex[h.ChunkIndex-1]; ok {
				p := f5bPickSentences(f5bSplitSentences(prev), h.Content, budget, true)
				if p != "" {
					b.WriteString(p)
					b.WriteByte('\n')
				}
			}
		}
		b.WriteString(h.Content)
		if useNext {
			if next, ok := chunkByIndex[h.ChunkIndex+1]; ok {
				n := f5bPickSentences(f5bSplitSentences(next), h.Content, budget, false)
				if n != "" {
					b.WriteByte('\n')
					b.WriteString(n)
				}
			}
		}
		out[i].Content = b.String()
	}
	return out
}

// f6FirstLineTitle 取块首非空行作合成标题：≤40 rune 且不含句末标点（像标题而非正文）。
func f6FirstLineTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rs := []rune(line)
		if len(rs) == 0 || len(rs) > 40 {
			return ""
		}
		for _, r := range line {
			if r == '。' || r == '！' || r == '？' || r == '；' || r == '，' {
				return ""
			}
		}
		return line
	}
	return ""
}

// 四种策略（函数签名匹配 f5RunSuite 的 enrichFn）
func f6PrevOnly(hits []storage.DocumentChunkHit, m map[int]string) []storage.DocumentChunkHit {
	return f6EnrichBase(hits, m, true, false, f5NeighborRunes, false)
}
func f6NextOnly(hits []storage.DocumentChunkHit, m map[int]string) []storage.DocumentChunkHit {
	return f6EnrichBase(hits, m, false, true, f5NeighborRunes, false)
}
func f6ShortNeighbors(hits []storage.DocumentChunkHit, m map[int]string) []storage.DocumentChunkHit {
	return f6EnrichBase(hits, m, true, true, 60, false)
}
func f6TitlePrefix(hits []storage.DocumentChunkHit, m map[int]string) []storage.DocumentChunkHit {
	return f6EnrichBase(hits, m, false, false, 0, true)
}

func TestF6EnrichStructBook2(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在，跳过（%s）", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F6 口径 = 云混合")
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

	// 共享 setup：本地 PDF → 生产提取器 → 建索引/向量（hash 幂等）
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

	strategies := []struct {
		name string
		fn   func([]storage.DocumentChunkHit, map[int]string) []storage.DocumentChunkHit
	}{
		{"B仅上文", f6PrevOnly},
		{"C仅下文", f6NextOnly},
		{"D短邻居", f6ShortNeighbors},
		{"E标题前缀", f6TitlePrefix},
	}
	for _, s := range strategies {
		t.Run(s.name, func(t *testing.T) {
			f5RunSuite(t, "Book2 "+s.name, st, app, user, doc.ID, cases, chunks, vhits, vrows, emb, rr, s.fn)
		})
	}
}

// TestF6EnrichStructBook1 D 短邻居策略的主书泛化观测（预注册判定线④）。
func TestF6EnrichStructBook1(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在，跳过（%s）", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F6 口径 = 云混合")
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
	f5RunSuite(t, "Book1 AgenticSE [F6 D短邻居]", st, app, user, mdoc.ID, cases, chunks, vhits, vrows, emb, rr, f6ShortNeighbors)
}
