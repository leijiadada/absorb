package document_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/document"
	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/retrieval"
	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
	fetch "absorb-agent/tool/fetch_page"
	searchdoc "absorb-agent/tool/search_document"
)

// f5_neighbor_context_test.go —— F5 预注册实验：精排输入邻居上下文富化。
//
// 单变量：喂给 reranker 的文档文本 孤立块 → [prev尾150字]\n[本块]\n[next头150字]。
// 其余（块边界/FTS/向量/RRF/融合榜/池构造/精排 query/裁剪）全部不动。
//
// 判定用 key（DocumentID#ChunkIndex）：富化文本只影响 reranker 打分，不参与命中判定，
// 避免邻居块恰好含 anchor 造成假命中。
//
// 预注册与判定线见 design/5.md「F5 预注册」。零生产代码改动。

const f5NeighborRunes = 150

type f5Case struct {
	name   string
	query  string
	anchor string
}

type f5Result struct {
	name       string
	rankPlain  int
	rankEnrich int
	msPlain    int64
	msEnrich   int64
	fallback   bool
	keyFound   bool
}

// f5EnrichHits 对池内每个 hit，按 ChunkIndex±1 取邻居块首尾拼成富化文本。
// 只改 Content 字段，DocumentID/ChunkIndex 保持（key 判定依赖）。
func f5EnrichHits(hits []storage.DocumentChunkHit, chunkByIndex map[int]string) []storage.DocumentChunkHit {
	out := make([]storage.DocumentChunkHit, len(hits))
	for i, h := range hits {
		out[i] = h
		var b strings.Builder
		if prev, ok := chunkByIndex[h.ChunkIndex-1]; ok {
			rs := []rune(prev)
			if len(rs) > f5NeighborRunes {
				rs = rs[len(rs)-f5NeighborRunes:]
			}
			b.WriteString(string(rs))
			b.WriteByte('\n')
		}
		b.WriteString(h.Content)
		if next, ok := chunkByIndex[h.ChunkIndex+1]; ok {
			rs := []rune(next)
			if len(rs) > f5NeighborRunes {
				rs = rs[:f5NeighborRunes]
			}
			b.WriteByte('\n')
			b.WriteString(string(rs))
		}
		out[i].Content = b.String()
	}
	return out
}

func f5RankOfKey(hits []storage.DocumentChunkHit, key string) int {
	for i, h := range hits {
		if h.DocumentID+"#"+strconv.Itoa(h.ChunkIndex) == key {
			return i + 1
		}
	}
	return 0
}

// f5RunSuite 双书共用的配对 runner：共享词法/向量/融合榜，只差喂给 reranker 的文本。
// enrichFn 决定富化策略（F5 机械截取 / F5b 句子边界+去重），是唯一变量。
func f5RunSuite(t *testing.T, label string, st storage.Storage, app, user, docID string,
	cases []f5Case, chunks []*storage.DocumentChunk, vhits []storage.DocumentChunkHit,
	vrows []storage.ChunkVectorRow, emb retrieval.Embedder, rr retrieval.Reranker,
	enrichFn func([]storage.DocumentChunkHit, map[int]string) []storage.DocumentChunkHit) {
	t.Helper()
	a := document.SharedAnalyzer()
	chunkByIndex := make(map[int]string, len(chunks))
	anchorKeys := make(map[string]string, len(cases)) // name → anchor block key
	for _, ch := range chunks {
		chunkByIndex[ch.ChunkIndex] = ch.Content
	}
	for _, c := range cases {
		for _, ch := range chunks {
			if strings.Contains(ch.Content, c.anchor) {
				anchorKeys[c.name] = docID + "#" + strconv.Itoa(ch.ChunkIndex)
				break
			}
		}
	}

	results := make([]f5Result, 0, len(cases))
	for _, c := range cases {
		out := f5Result{name: c.name}
		key, ok := anchorKeys[c.name]
		out.keyFound = ok
		if !ok {
			t.Fatalf("[%s] 锚点块 key 未找到（case=%s）", label, c.name)
		}
		lex, err := st.SearchDocumentChunks(context.Background(), app, user, a.QueryExpression(c.query), []string{docID}, 50)
		if err != nil {
			t.Fatalf("[%s] lex %s: %v", label, c.name, err)
		}
		qv, err := emb.EmbedQuery(context.Background(), c.query)
		if err != nil {
			t.Fatalf("[%s] embed %s: %v", label, c.name, err)
		}
		vec50 := retrieval.TopKByCosine(vhits, vrows, qv, 50)
		fused := retrieval.FuseRRF(lex, vec50, 50)
		// rerankPoolF4B 不内部截断——显式取 seed40，与生产 RerankSeedTopN 一致
		vec40 := vec50
		if len(vec40) > retrieval.RerankSeedTopN {
			vec40 = vec40[:retrieval.RerankSeedTopN]
		}
		pool, poolKeys := rerankPoolF4B(fused, vec40)

		// plain 腿：原文本
		t0 := time.Now()
		rrP, appliedP := retrieval.RerankCandidates(context.Background(), pool, c.query, rr, len(pool))
		out.msPlain = time.Since(t0).Milliseconds()
		out.fallback = !appliedP
		finalP := append(append([]storage.DocumentChunkHit{}, rrP...), f5FusedTail(fused, poolKeys)...)
		dedupP, _ := searchdoc.DeduplicateHits(finalP, 10)
		out.rankPlain = f5RankOfKey(dedupP, key)

		// enrich 腿：邻居富化文本（唯一变量）
		poolEnriched := enrichFn(pool, chunkByIndex)
		t1 := time.Now()
		rrE, appliedE := retrieval.RerankCandidates(context.Background(), poolEnriched, c.query, rr, len(poolEnriched))
		out.msEnrich = time.Since(t1).Milliseconds()
		if !appliedE {
			out.fallback = true
		}
		finalE := append(append([]storage.DocumentChunkHit{}, rrE...), f5FusedTail(fused, poolKeys)...)
		dedupE, _ := searchdoc.DeduplicateHits(finalE, 10)
		out.rankEnrich = f5RankOfKey(dedupE, key)

		results = append(results, out)
	}

	// 汇总（同义组判定，对照组单列）
	var synN, hitP5, hitE5, lost, gained float64
	var msP, msE int64
	t.Logf("%-24s %-8s %-8s %s", "题目", "plain", "enrich", "ms P/E")
	for _, r := range results {
		t.Logf("%-24s %-8s %-8s %d/%d", r.name, rankLabel(r.rankPlain), rankLabel(r.rankEnrich), r.msPlain, r.msEnrich)
		isSyn := !strings.HasPrefix(r.name, "对照")
		if !isSyn {
			continue
		}
		synN++
		p5, e5 := r.rankPlain > 0 && r.rankPlain <= 5, r.rankEnrich > 0 && r.rankEnrich <= 5
		if p5 {
			hitP5++
		}
		if e5 {
			hitE5++
		}
		if p5 && !e5 {
			lost++
		}
		if !p5 && e5 {
			gained++
		}
		msP += r.msPlain
		msE += r.msEnrich
	}
	t.Logf("")
	t.Logf("==== F5 %s（%d 同义题，单轮，key 判定）====", label, int(synN))
	t.Logf("plain R@5=%.0f%%  enrich R@5=%.0f%%  Δ=%+.0fpp", hitP5/synN*100, hitE5/synN*100, (hitE5-hitP5)/synN*100)
	t.Logf("获救=%.0f  丢失=%.0f  精排延迟 plain=%dms enrich=%dms", gained, lost, msP/int64(synN), msE/int64(synN))
}

func f5FusedTail(fused []storage.DocumentChunkHit, poolKeys map[string]struct{}) []storage.DocumentChunkHit {
	var tail []storage.DocumentChunkHit
	for _, h := range fused[rrTopN:] {
		if _, ok := poolKeys[chunkKeyOf(h)]; !ok {
			tail = append(tail, h)
		}
	}
	return tail
}

func TestF5NeighborContextBook2(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在，跳过（%s）", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F5 口径 = 云混合；设置 ABSORB_CLOUD_EMBED=1 后运行")
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

	// 自包含语料（与 book2_eval 同款幂等路径）
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
	const (
		app, user = "absorb-reader", "absorb-user"
	)
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
	f5RunSuite(t, "Book2 人月神话", st, app, user, doc.ID, cases, chunks, vhits, vrows, emb, rr, f5EnrichHits)
}

func TestF5NeighborContextBook1(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在，跳过（%s）", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F5 口径 = 云混合；设置 ABSORB_CLOUD_EMBED=1 后运行")
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
	const (
		app, user = "absorb-reader", "absorb-user"
	)
	// 用主书原本文档（URL 无后缀）：冻结同义集锚点即出自该书，且开发库副本中
	// 带索引/向量的是主文档；?poppler-eval=1 镜像文档在副本里无索引（F5 首轮
	// 因选错 URL 被 Skip）——此处修正为原文档。
	mdoc, _ := st.UpsertDocument(ctx, app, user, "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf", "AgenticSE Book CN")
	state, _ := st.LoadDocumentIndexState(ctx, app, user, mdoc.ID)
	if state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Skip("主书原文档未就绪")
	}
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	rr := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
	vhits, vrows, err := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{mdoc.ID})
	if err != nil || len(vrows) == 0 {
		t.Skipf("主书镜像向量缺失: %v", err)
	}
	chunks, _ := st.LoadDocumentChunks(ctx, app, user, mdoc.ID)
	cases := make([]f5Case, 0, len(synonymCases))
	for _, c := range synonymCases {
		cases = append(cases, f5Case{c.name, c.query, c.anchor})
	}
	f5RunSuite(t, "Book1 AgenticSE", st, app, user, mdoc.ID, cases, chunks, vhits, vrows, emb, rr, f5EnrichHits)
}
