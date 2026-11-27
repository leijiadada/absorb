package document_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	searchdoc "absorb-agent/tool/search_document"
)

// f7_breadcrumb_prefix_test.go —— F7：真实面包屑前缀（HeadingMark SectionPath）配对实验。
// 预注册见 design/5.md。主书单书实验（Book2 poppler marks=0，无对应形态）。
// 零生产改动；key 判定；专用 runner（因需从 chunk 读 SectionPath，不复用 f5RunSuite 的字符串 map）。

const f7MarksSuffix = "?marks-ftsonly-eval=1"

const f7BookURL = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"

func TestF7BreadcrumbPrefix(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在，跳过（%s）", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F7 口径 = 云混合")
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
	proxyFn := func() string { return os.Getenv("ABSORB_PROXY_URL") }

	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const (
		app, user = "absorb-reader", "absorb-user"
	)

	// 1. 语料：网络抓 PDF → poppler 提取 → docling marks（F3 现成路径）
	data, err := fetchBytesForTest(ctx, proxyFn, f7BookURL, 50<<20)
	if err != nil {
		t.Skipf("下载语料失败（网络不可达），跳过: %v", err)
	}
	popplerText, err := extract.PopplerText(context.Background(), popplerBin(t), data)
	if err != nil {
		t.Fatalf("PopplerText: %v", err)
	}
	marks, injected := extract.MarksForDiag(data, popplerText)
	if injected == 0 {
		t.Fatal("marks 命中 0——无法测面包屑前缀")
	}
	marksJSON, _ := json.Marshal(marks)
	t.Logf("marks：%d 字语料，命中 %d 个章节标题", len([]rune(popplerText)), injected)

	// 2. 建两个镜像：plain（无 marks）与 marks（真面包屑进 SectionPath）
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	retrieval.SetSharedEmbedder(emb)
	rr := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
	svc := document.NewService(st, nil, app, user).WithEmbedder(emb)
	build := func(suffix, headingMarks string) *storage.Document {
		mdc := &storage.DocumentContent{
			URL: f7BookURL + suffix, ContentText: popplerText, CharCount: len([]rune(popplerText)),
			ContentType: "application/pdf", ByteSize: int64(len(popplerText)),
			HeadingMarks: headingMarks, ExtractionStatus: extract.StatusReady,
			FetchedAt: time.Now(),
		}
		sum := sha256.Sum256([]byte(popplerText))
		mdc.ContentHash = hex.EncodeToString(sum[:])
		mdoc, err := st.UpsertDocument(ctx, app, user, mdc.URL, "AgenticSE "+suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveDocumentContent(ctx, app, user, mdc); err != nil {
			t.Fatal(err)
		}
		svc.BuildIndex(ctx, mdoc.ID, mdc)
		return mdoc
	}
	plainDoc := build(popplerMirrorSuffix, "")
	marksDoc := build(f7MarksSuffix, string(marksJSON))

	// 3. 面包屑覆盖断言（≥99%）
	marksChunks, _ := st.LoadDocumentChunks(ctx, app, user, marksDoc.ID)
	withSP := 0
	for _, ch := range marksChunks {
		if ch.SectionPath != "" {
			withSP++
		}
	}
	coverage := float64(withSP) / float64(len(marksChunks)) * 100
	t.Logf("面包屑覆盖：%d/%d = %.1f%%", withSP, len(marksChunks), coverage)
	if coverage < 99 {
		t.Fatalf("面包屑覆盖 %.1f%% < 99%% 门槛", coverage)
	}

	vhits, vrows, err := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{marksDoc.ID})
	if err != nil || len(vrows) == 0 {
		t.Fatalf("marks 镜像向量缺失: %v", err)
	}
	// plain 腿向量：marks 与 plain 正文相同、chunk 边界相同 → 向量内容一致，
	// 用 marks 镜像的 vhits/vrows 但按 plain docID 检索即可（TopKByCosine 内按 ChunkIndex）。
	plainChunks, _ := st.LoadDocumentChunks(ctx, app, user, plainDoc.ID)
	if len(plainChunks) != len(marksChunks) {
		t.Fatalf("块数漂移 plain=%d marks=%d", len(plainChunks), len(marksChunks))
	}

	// 4. 配对 runner：plain vs 真面包屑前缀
	a := document.SharedAnalyzer()
	type info struct{ content, sp string }
	marksInfo := map[int]info{}
	for _, ch := range marksChunks {
		marksInfo[ch.ChunkIndex] = info{ch.Content, ch.SectionPath}
	}
	anchorKeys := map[string]string{}
	for _, c := range synonymCases {
		for _, ch := range marksChunks {
			if strings.Contains(ch.Content, c.anchor) {
				anchorKeys[c.name] = marksDoc.ID + "#" + fmt.Sprint(ch.ChunkIndex)
				break
			}
		}
	}

	var synN, hitP, hitE, lost, gained float64
	t.Logf("%-24s %-9s %-9s", "题目", "plain", "breadcrumb")
	for _, c := range synonymCases {
		key, ok := anchorKeys[c.name]
		if !ok {
			t.Fatalf("锚点块未找到: %s", c.name)
		}
		lex, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.query), []string{plainDoc.ID}, 50)
		if err != nil {
			t.Fatal(err)
		}
		qv, err := emb.EmbedQuery(ctx, c.query)
		if err != nil {
			t.Fatal(err)
		}
		vec50 := retrieval.TopKByCosine(vhits, vrows, qv, 50)
		fused := retrieval.FuseRRF(lex, vec50, 50)
		vec40 := vec50
		if len(vec40) > retrieval.RerankSeedTopN {
			vec40 = vec40[:retrieval.RerankSeedTopN]
		}
		pool, poolKeys := rerankPoolF4B(fused, vec40)

		// plain 腿
		rrP, _ := retrieval.RerankCandidates(ctx, pool, c.query, rr, len(pool))
		finalP := append(append([]storage.DocumentChunkHit{}, rrP...), f5FusedTail(fused, poolKeys)...)
		dp, _ := searchdoc.DeduplicateHits(finalP, 10)
		rankP := f5RankOfKey(dp, key)

		// breadcrumb 腿：Content 前拼真实 SectionPath（唯一变量）
		enriched := make([]storage.DocumentChunkHit, len(pool))
		copy(enriched, pool)
		for i, h := range enriched {
			if inf, ok := marksInfo[h.ChunkIndex]; ok && inf.sp != "" {
				enriched[i].Content = inf.sp + "\n" + h.Content
			}
		}
		rrE, _ := retrieval.RerankCandidates(ctx, enriched, c.query, rr, len(enriched))
		finalE := append(append([]storage.DocumentChunkHit{}, rrE...), f5FusedTail(fused, poolKeys)...)
		de, _ := searchdoc.DeduplicateHits(finalE, 10)
		rankE := f5RankOfKey(de, key)

		if strings.HasPrefix(c.name, "对照") {
			continue
		}
		synN++
		p5, e5 := rankP > 0 && rankP <= 5, rankE > 0 && rankE <= 5
		if p5 {
			hitP++
		}
		if e5 {
			hitE++
		}
		if p5 && !e5 {
			lost++
		}
		if !p5 && e5 {
			gained++
		}
		t.Logf("%-24s %-9s %-9s", c.name, rankLabel(rankP), rankLabel(rankE))
	}
	t.Logf("")
	t.Logf("==== F7 真实面包屑前缀（主书 44 同义题，覆盖 %.1f%%）====", coverage)
	t.Logf("plain R@5=%.0f%%  breadcrumb R@5=%.0f%%  Δ=%+.0fpp", hitP/synN*100, hitE/synN*100, (hitE-hitP)/synN*100)
	t.Logf("获救=%.0f 丢失=%.0f", gained, lost)
}
