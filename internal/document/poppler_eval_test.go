package document_test

// poppler_eval_test.go 择优版 Phase 1 验收（PDF 提取双引擎 A/B，冻结集复用）：
//
// 1. TestPopplerRescueBooks——两本主路乱码拒绝入库的书（《人月神话》文字版、
//    《程序员的自我修养》）经 poppler 第二引擎救回：全文可读性断言 + 建索引 +
//    词法检索可达性冒烟。
// 2. TestSynonymRetrievalPopplerAB——AgenticSE 分身（?poppler-eval=1）用生产
//    择优提取器产出的文本建索引，跑冻结 48 题（synonymCases 原样复用，评测集
//    本身不动）同口径管线，与冻结基线对比：
//      词法：        23% / 30% / MRR 0.20
//      云混合+精排： 45% / 45% / MRR 0.34
//    判读规则同冻结文件头（<4pp 噪声内持平，≥4pp 方可下方向性结论）。
//
// 与冻结测试的差异只有两处：语料文本来自生产择优提取器（Phase 1 起对本书
// 会自动切到 poppler 文本），检索范围限定分身文档（锚点两份语料都有，不限定
// 会互相污染排名）。
//
// 运行（同冻结测试口径）：
//
//	go test ./internal/document/ -run "TestPoppler" -v
//	# 云混合口径：
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_EMB_API_KEY="<key>"
//	$env:ABSORB_EMB_MODEL="Qwen/Qwen3-Embedding-4B"; $env:ABSORB_RR_MODEL="Qwen/Qwen3-Reranker-4B"
//
// 前置：absorb-m1-check.db 副本（缺失 Skip）；pdftotext 可用（PATH 或
// ABSORB_PDFTOTEXT，缺失 Skip）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"absorb-agent/internal/document"
	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/retrieval"
	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
	fetch_page "absorb-agent/tool/fetch_page"
	searchdoc "absorb-agent/tool/search_document"
)

const popplerMirrorSuffix = "?poppler-eval=1"

// rrTopN 精排候选数（与冻结测试默认值一致）。
const rrTopN = 16

// popplerBin 解析 pdftotext 路径（与 extract.pdftotextBin 同逻辑，测试侧独立实现）。
func popplerBin(t *testing.T) string {
	t.Helper()
	if p := strings.TrimSpace(os.Getenv("ABSORB_PDFTOTEXT")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Skipf("ABSORB_PDFTOTEXT 指向的 pdftotext 不存在: %s", p)
	}
	if path, err := exec.LookPath("pdftotext"); err == nil {
		return path
	}
	t.Skip("pdftotext 不可用（未装 Poppler 且未设 ABSORB_PDFTOTEXT），跳过")
	return ""
}

// fetchBytesForTest 下载原始 PDF 字节（与生产 fetchBytes 同款代理解析链）。
func fetchBytesForTest(ctx context.Context, proxyFn func() string, url string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = fetch_page.ProxyResolver(proxyFn)
	client := &http.Client{Transport: tr, Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("下载失败 status=" + strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("原始字节超上限")
	}
	return data, nil
}

// garbledRatioFor 乱码率（与 extract.garbledRatio 同口径：U+FFFD + 控制符 +
// 私有区占全部字符的比例），测试侧独立实现。
func garbledRatioFor(s string) float64 {
	total, bad := 0, 0
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		total++
		if r == unicode.ReplacementChar || (r < 0x20 && r != 0) || (r >= 0xE000 && r <= 0xF8FF) {
			bad++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(bad) / float64(total)
}

// findRepoPathFrom 从当前目录向上逐级查找仓库内相对路径。
func findRepoPathFrom(rel string) string {
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(dir + "/" + rel); err == nil {
			return dir + "/" + rel
		}
		dir += "/.."
	}
	return ""
}

// TestPopplerRescueBooks 两本主路拒入库的书经 poppler 引擎救回：全文可读、
// 建索引成功、检索可达。
func TestPopplerRescueBooks(t *testing.T) {
	bin := popplerBin(t)
	dbPath := filepath.Join(os.TempDir(), "absorb-poppler-rescue.db")
	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	const (
		app  = "absorb-reader"
		user = "absorb-user"
	)
	svc := document.NewService(st, nil, app, user) // BuildIndex 只消费文本
	for _, book := range []struct {
		file  string
		title string
		probe string // 检索冒烟目标词（书内概念词）
	}{
		{"docs/pdf/人月神话-中文-目录-文字版.pdf", "人月神话（文字版，poppler 救回）", "外科手术队伍"},
		{"docs/pdf/程序员的自我修养链接装载与库.pdf", "程序员的自我修养（poppler 救回）", "动态链接"},
	} {
		path := findRepoPathFrom(book.file)
		if path == "" {
			t.Skipf("样书不存在（%s），跳过", book.file)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", book.file, err)
		}
		text, err := extract.PopplerText(bin, data)
		if err != nil {
			t.Fatalf("PopplerText %s: %v", book.file, err)
		}
		rs := []rune(text)
		if len(rs) < 100000 {
			t.Fatalf("%s 文本量不足：%d rune", book.file, len(rs))
		}
		if r := garbledRatioFor(text); r > 0.005 {
			t.Fatalf("%s 乱码率超标：%.4f", book.file, r)
		}
		if !strings.Contains(text, book.probe) {
			t.Fatalf("%s 抽检词 %q 未命中", book.file, book.probe)
		}

		// 落库 + 建索引（与生产 Prepare 落库三步同构，URL 用本地文件锚点）
		url := "file:///" + strings.ReplaceAll(strings.TrimPrefix(path, `\\`), `\`, "/")
		doc, err := st.UpsertDocument(ctx, app, user, url, book.title)
		if err != nil {
			t.Fatalf("upsert %s: %v", book.file, err)
		}
		sum := sha256.Sum256([]byte(text))
		dc := &storage.DocumentContent{
			URL: url, ContentText: text,
			CharCount: len(rs), ContentType: "application/pdf",
			ByteSize: int64(len(text)), ContentHash: hex.EncodeToString(sum[:]),
			ExtractionStatus: extract.StatusReady, ExtractionVersion: 2,
			FetchedAt: time.Now(),
		}
		if err := st.SaveDocumentContent(ctx, app, user, dc); err != nil {
			t.Fatalf("save %s: %v", book.file, err)
		}
		svc.BuildIndex(ctx, doc.ID, dc)
		state, _ := st.LoadDocumentIndexState(ctx, app, user, doc.ID)
		if state == nil || state.IndexStatus != storage.IndexStatusReady || state.ChunkCount < 100 {
			t.Fatalf("%s 索引未就绪: %+v", book.file, state)
		}
		// 检索可达性冒烟：probe 词必须能从索引召回
		a := document.SharedAnalyzer()
		hits, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(book.probe), []string{doc.ID}, 10)
		if err != nil {
			t.Fatalf("search %s: %v", book.file, err)
		}
		found := false
		for _, h := range hits {
			if strings.Contains(h.Content, book.probe) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s 抽检词 %q 索引召回失败", book.file, book.probe)
		}
		t.Logf("[救回] %s：%d 字 → %d chunks，%q 可达", book.title, len(rs), state.ChunkCount, book.probe)
	}
}

// TestSynonymRetrievalPopplerAB 冻结 48 题 × poppler 语料 A/B（口径与冻结
// 测试一致，检索范围限定分身文档）。云模式未配置时自动降级为词法口径，
// 对应冻结基线 23%/30%/0.20。
func TestSynonymRetrievalPopplerAB(t *testing.T) {
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

	const (
		bookURL = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"
		app     = "absorb-reader"
		user    = "absorb-user"
	)
	dc, err := st.LoadDocumentContent(ctx, app, user, bookURL)
	if err != nil || dc == nil {
		t.Skipf("副本中无目标文档正文 (err=%v)", err)
	}

	cloud := os.Getenv("ABSORB_CLOUD_EMBED") == "1"
	proxyFn := func() string { return os.Getenv("ABSORB_PROXY_URL") }

	// 语料文本直接经 PopplerText 产出（测试只关心 poppler 语料，不关心择优
	// 结果——择优随环境漂移，A/B 语料必须确定性）；字节下载走与生产一致的
	// 代理解析链
	data, err := fetchBytesForTest(ctx, proxyFn, bookURL, 50<<20)
	if err != nil {
		t.Skipf("下载语料失败（网络不可达），跳过: %v", err)
	}
	popplerText, err := extract.PopplerText(bin, data)
	if err != nil {
		t.Fatalf("PopplerText: %v", err)
	}

	// 锚点自检：评测集锚点必须在新语料中真实存在（Phase 0 外部验证 48/48）
	for _, c := range synonymCases {
		if !strings.Contains(popplerText, c.anchor) {
			t.Fatalf("锚点 %q 不在 poppler 语料中（题目 %s）——评测集与语料不兼容", c.anchor, c.name)
		}
	}

	// 语义路（与冻结测试同款云装配）
	var emb retrieval.Embedder
	var reranker retrieval.Reranker
	rrTopN := 16
	hybrid := false
	if cloud {
		embKey := os.Getenv("ABSORB_EMB_API_KEY")
		if embKey == "" {
			t.Fatal("cloud 模式需要 ABSORB_EMB_API_KEY")
		}
		embModel := os.Getenv("ABSORB_EMB_MODEL")
		if embModel == "" {
			embModel = "Qwen/Qwen3-Embedding-4B"
		}
		emb = retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
		retrieval.SetSharedEmbedder(emb)
		rrModel := os.Getenv("ABSORB_RR_MODEL")
		if rrModel == "" {
			rrModel = "Qwen/Qwen3-Reranker-4B"
		}
		reranker = retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
		hybrid = true
		t.Logf("mode = HYBRID（云 %s + 云精排 %s，topN=%d）", embModel, rrModel, rrTopN)
	} else {
		t.Log("mode = 词法基线（未启用 ABSORB_CLOUD_EMBED），对比冻结词法基线 23%/30%/0.20")
	}

	// 分身文档：poppler 文本落库 + 建索引（词法 + 可选向量）
	mirrorURL := bookURL + popplerMirrorSuffix
	mdc := *dc
	mdc.URL = mirrorURL
	mdc.ContentText = popplerText
	mdc.CharCount = len([]rune(popplerText))
	sum := sha256.Sum256([]byte(popplerText))
	mdc.ContentHash = hex.EncodeToString(sum[:])
	mdc.ByteSize = int64(len(popplerText))
	mdoc, err := st.UpsertDocument(ctx, app, user, mirrorURL, "AgenticSE Book CN (poppler)")
	if err != nil {
		t.Fatalf("upsert mirror: %v", err)
	}
	if err := st.SaveDocumentContent(ctx, app, user, &mdc); err != nil {
		t.Fatalf("save mirror content: %v", err)
	}
	svc := document.NewService(st, nil, app, user).WithEmbedder(emb)
	started := time.Now()
	svc.BuildIndex(ctx, mdoc.ID, &mdc)
	state, _ := st.LoadDocumentIndexState(ctx, app, user, mdoc.ID)
	if state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Fatalf("mirror index not ready: %+v", state)
	}
	if hybrid {
		if _, vrows, verr := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{mdoc.ID}); verr != nil || len(vrows) == 0 {
			t.Fatalf("分身向量未入库 (err=%v, rows=%d)——HYBRID 评测无效", verr, len(vrows))
		}
	}
	t.Logf("poppler 分身：%d 字 → %d chunks（%s）", mdc.CharCount, state.ChunkCount,
		time.Since(started).Round(time.Millisecond))

	// 48 题同口径评分（检索范围限定分身，防止原语料分身互相污染排名）
	modeName := "词法检索（poppler 语料）"
	if hybrid {
		modeName = "HYBRID 混合检索（poppler 语料，词法 + " + emb.Model() + "，RRF）+ Reranker"
	}
	r5, r10, mrr := scoreSynonymCases(ctx, t, st, app, user, mdoc.ID, hybrid, emb, reranker)
	t.Logf("==== %s 同义评测（冻结 48 题 A/B）====", modeName)
	t.Logf("同义组：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", r5*100, r10*100, mrr)
	t.Logf("冻结基线：词法 23%%/30%%/0.20 | 云混合 45%%/45%%/0.34（判读：<4pp 噪声内持平）")
}

// scoreSynonymCases 冻结 48 题同口径评分（对照题只做锚点宽召回校验，不计入
// 同义组指标）。返回同义组 Recall@5 / Recall@10 / MRR@10（0~1）。
func scoreSynonymCases(ctx context.Context, t *testing.T, st storage.Storage,
	app, user, docID string, hybrid bool, emb retrieval.Embedder, reranker retrieval.Reranker,
) (float64, float64, float64) {
	t.Helper()
	a := document.SharedAnalyzer()
	var (
		recallAt5, recallAt10 float64
		mrrSum                float64
		synN                  float64
	)
	for _, c := range synonymCases {
		lexical, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.query), []string{docID}, 50)
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		hits := lexical
		if hybrid && emb != nil {
			if qv, qerr := emb.EmbedQuery(ctx, c.query); qerr == nil {
				if vhits, vrows, verr := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{docID}); verr == nil {
					vector := retrieval.TopKByCosine(vhits, vrows, qv, 50)
					fused := retrieval.FuseRRF(lexical, vector, 50)
					if reranker != nil {
						fused, _ = retrieval.RerankCandidates(ctx, fused, c.query, reranker, rrTopN)
					}
					hits, _ = searchdoc.DeduplicateHits(fused, 10)
				}
			}
		} else {
			hits, _ = searchdoc.DeduplicateHits(lexical, 10)
		}
		rank := 0
		for i, h := range hits {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				rank = i + 1
				break
			}
		}
		if !strings.HasPrefix(c.name, "对照") {
			synN++
			if rank > 0 {
				mrrSum += 1.0 / float64(rank)
				if rank <= 5 {
					recallAt5++
				}
				recallAt10++
			}
			t.Logf("%-30s %s", c.name, rankLabel(rank))
			continue
		}
		// 对照组校验（同冻结口径）：问句查询只记录；可达性要求走锚点
		// 自身查询 + 宽召回（limit 200）——问句 BM25 竞争激烈不等于索引异常
		res, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.anchor), []string{docID}, 200)
		if err != nil {
			t.Fatalf("reachability search %s: %v", c.name, err)
		}
		reachable := false
		for _, h := range res {
			if strings.Contains(h.Content, c.anchor) {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("对照组 %s 锚点 chunk 无法召回——语料评测有效性存疑", c.name)
		}
		t.Logf("%-30s %s（锚点可达=%v）", c.name, rankLabel(rank), reachable)
	}
	if synN == 0 {
		t.Fatal("同义题为空")
	}
	return recallAt5 / synN, recallAt10 / synN, mrrSum / synN
}

// TestSynonymRetrievalStructAB 生产端到端腿：生产择优提取器（双跑择优 +
// marks 门控）产出的正文建索引，同口径评分。P1 修订：AgenticSE 的 dslipak
// 主路文本带行内乱序（罕见 bigram 42.3% vs 32.7%），择优必然切到 poppler
// 干净文本——门控下 marks 不生成（干净文本上旧叠加实测稳定 -7pp，撤门控
// 与否由 F3a marks 评测决定），本腿验证生产端到端（择优换源 + 门控跳过）
// 的实际产出，指标应≈poppler 纯文本。
func TestSynonymRetrievalStructAB(t *testing.T) {
	popplerBin(t) // pdftotext 可用性门控（生产择优依赖它）
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

	const (
		bookURL = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"
		app     = "absorb-reader"
		user    = "absorb-user"
	)
	dc, err := st.LoadDocumentContent(ctx, app, user, bookURL)
	if err != nil || dc == nil {
		t.Skipf("副本中无目标文档正文 (err=%v)", err)
	}

	cloud := os.Getenv("ABSORB_CLOUD_EMBED") == "1"
	proxyFn := func() string { return os.Getenv("ABSORB_PROXY_URL") }
	var emb retrieval.Embedder
	var reranker retrieval.Reranker
	hybrid := false
	if cloud {
		embKey := os.Getenv("ABSORB_EMB_API_KEY")
		if embKey == "" {
			t.Fatal("cloud 模式需要 ABSORB_EMB_API_KEY")
		}
		embModel := os.Getenv("ABSORB_EMB_MODEL")
		if embModel == "" {
			embModel = "Qwen/Qwen3-Embedding-4B"
		}
		emb = retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
		retrieval.SetSharedEmbedder(emb)
		rrModel := os.Getenv("ABSORB_RR_MODEL")
		if rrModel == "" {
			rrModel = "Qwen/Qwen3-Reranker-4B"
		}
		reranker = retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
		hybrid = true
	}

	// 生产端到端：双跑择优（乱序项判切）+ 叠加门控（含两次下载与 docling 解析）
	fetchSvc := fetch_page.NewFetchPageService(proxyFn, nil)
	prod, err := extract.NewPDFExtractor(fetchSvc, proxyFn, func() int64 { return 50 << 20 }).Extract(ctx, bookURL)
	if err != nil {
		t.Fatalf("生产提取器失败: %v", err)
	}
	injected := len(markdownHeadingLines(prod.Content))
	chars := len([]rune(prod.Content))
	t.Logf("生产端到端：%d 字，注入标题 %d 个，marks %d 个", chars, injected, len(prod.Marks))
	if injected != 0 {
		t.Fatalf("门控失效：本书主路带行内乱序应切到 poppler（不注入标题），实际注入 %d 个", injected)
	}
	if len(prod.Marks) != 0 {
		t.Fatalf("门控失效：本书主路应切到 poppler（不生成 marks），实际 %d 个", len(prod.Marks))
	}
	if chars < 210000 {
		// v4 全文 226K；v5 起 TOC 剥离删约 8.7K 目录字符 → ~217K。主路未剥离
		// 文本 ≈210K，用 210K 门区分「剥离后的 poppler 全文」与「主路文本」。
		t.Fatalf("语料字数异常（应≈剥离目录后的 poppler 全文 ~217K）：%d", chars)
	}

	// 锚点自检
	for _, c := range synonymCases {
		if !strings.Contains(prod.Content, c.anchor) {
			t.Fatalf("锚点 %q 不在结构叠加语料中（题目 %s）", c.anchor, c.name)
		}
	}

	// 分身落库 + 建索引
	mirrorURL := bookURL + "?poppler-struct-eval=1"
	mdc := *dc
	mdc.URL = mirrorURL
	mdc.ContentText = prod.Content
	mdc.CharCount = len([]rune(prod.Content))
	sum := sha256.Sum256([]byte(prod.Content))
	mdc.ContentHash = hex.EncodeToString(sum[:])
	mdc.ByteSize = int64(len(prod.Content))
	mdoc, err := st.UpsertDocument(ctx, app, user, mirrorURL, "AgenticSE Book CN (poppler+struct)")
	if err != nil {
		t.Fatalf("upsert struct mirror: %v", err)
	}
	if err := st.SaveDocumentContent(ctx, app, user, &mdc); err != nil {
		t.Fatalf("save struct mirror: %v", err)
	}
	svc := document.NewService(st, nil, app, user).WithEmbedder(emb)
	svc.BuildIndex(ctx, mdoc.ID, &mdc)
	state, _ := st.LoadDocumentIndexState(ctx, app, user, mdoc.ID)
	if state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Fatalf("struct mirror index not ready: %+v", state)
	}

	modeName := "词法检索（生产端到端语料：poppler 换源 + marks 门控跳过）"
	if hybrid {
		modeName = "HYBRID 混合检索（生产端到端语料，词法 + " + emb.Model() + "，RRF）+ Reranker"
	}
	r5, r10, mrr := scoreSynonymCases(ctx, t, st, app, user, mdoc.ID, hybrid, emb, reranker)
	t.Logf("==== %s 同义评测（冻结 48 题 A/B）====", modeName)
	t.Logf("同义组：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", r5*100, r10*100, mrr)
	t.Logf("冻结基线：词法 23%%/30%%/0.20 | 云混合 45%%/45%%/0.34 | poppler 纯文本 50~52%%/0.31")
}

// TestSynonymRetrievalMarksAB F3a→F3c 结构元数据对照（2026-11-18）：plain
// 镜像与 marks 镜像共享同一份 poppler 正文——块边界/分词/块内容逐位一致；
// 向量输入统一为纯正文（F3c 定稿：embeddingInput 去面包屑），两腿向量同
// 分布，唯一差异是 FTS section_path 加权元数据。断言块边界等价不变式
// （chunk 数一致）与面包屑覆盖 ≥99% 后，同口径 48 题评分。
// （F3b 软边界切块两轮 -5pp 未达标回滚，恢复本形态；实验记录见 design/5.md。）
//
// 预注册规则（F3c）：marks 两轮云混合均值 Recall@5 ≥ 52% 且 MRR ≥ 0.41
// 且配对 Δ ≥ -2pp（plain 不劣）——已于 2026-11-18 通过转正（57%/0.415，Δ=0）。
func TestSynonymRetrievalMarksAB(t *testing.T) {
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

	const (
		bookURL = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"
		app     = "absorb-reader"
		user    = "absorb-user"
	)
	dc, err := st.LoadDocumentContent(ctx, app, user, bookURL)
	if err != nil || dc == nil {
		t.Skipf("副本中无目标文档正文 (err=%v)", err)
	}

	cloud := os.Getenv("ABSORB_CLOUD_EMBED") == "1"
	proxyFn := func() string { return os.Getenv("ABSORB_PROXY_URL") }
	var emb retrieval.Embedder
	var reranker retrieval.Reranker
	hybrid := false
	if cloud {
		embKey := os.Getenv("ABSORB_EMB_API_KEY")
		if embKey == "" {
			t.Fatal("cloud 模式需要 ABSORB_EMB_API_KEY")
		}
		embModel := os.Getenv("ABSORB_EMB_MODEL")
		if embModel == "" {
			embModel = "Qwen/Qwen3-Embedding-4B"
		}
		emb = retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
		retrieval.SetSharedEmbedder(emb)
		rrModel := os.Getenv("ABSORB_RR_MODEL")
		if rrModel == "" {
			rrModel = "Qwen/Qwen3-Reranker-4B"
		}
		reranker = retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
		hybrid = true
		t.Logf("mode = HYBRID（云 %s + 云精排 %s）", embModel, rrModel)
	}

	// 语料（确定性）：poppler 纯文本 + docling 章节匹配产出 marks
	data, err := fetchBytesForTest(ctx, proxyFn, bookURL, 50<<20)
	if err != nil {
		t.Skipf("下载语料失败（网络不可达），跳过: %v", err)
	}
	popplerText, err := extract.PopplerText(bin, data)
	if err != nil {
		t.Fatalf("PopplerText: %v", err)
	}
	marks, injected := extract.MarksForDiag(data, popplerText)
	if injected == 0 {
		t.Fatal("marks 命中 0 个——F3a 对照无意义")
	}
	ors := []rune(popplerText)
	for _, m := range marks { // 偏移不变式全量断言（Offset 为 rune 口径）
		if !strings.HasPrefix(string(ors[m.Offset:]), m.Title) {
			t.Fatalf("mark 偏移不变式破坏：%q @ %d", m.Title, m.Offset)
		}
	}
	marksJSON, err := json.Marshal(marks)
	if err != nil {
		t.Fatalf("marshal marks: %v", err)
	}
	t.Logf("marks：%d 字语料，命中 %d 个章节标题", len([]rune(popplerText)), injected)

	svc := document.NewService(st, nil, app, user).WithEmbedder(emb)
	buildMirror := func(headingMarks, suffix, title string) *storage.Document {
		mirrorURL := bookURL + suffix
		mdc := *dc
		mdc.URL = mirrorURL
		mdc.ContentText = popplerText
		mdc.CharCount = len([]rune(popplerText))
		mdc.HeadingMarks = headingMarks
		sum := sha256.Sum256([]byte(popplerText))
		mdc.ContentHash = hex.EncodeToString(sum[:])
		mdc.ByteSize = int64(len(popplerText))
		mdoc, err := st.UpsertDocument(ctx, app, user, mirrorURL, title)
		if err != nil {
			t.Fatalf("upsert %s: %v", title, err)
		}
		if err := st.SaveDocumentContent(ctx, app, user, &mdc); err != nil {
			t.Fatalf("save %s: %v", title, err)
		}
		svc.BuildIndex(ctx, mdoc.ID, &mdc)
		state, _ := st.LoadDocumentIndexState(ctx, app, user, mdoc.ID)
		if state == nil || state.IndexStatus != storage.IndexStatusReady {
			t.Fatalf("%s index not ready: %+v", title, state)
		}
		return mdoc
	}
	mdocPlain := buildMirror("", popplerMirrorSuffix, "AgenticSE Book CN (poppler)")
	// 复用既有 marks-ftsonly 镜像（F3c 定稿形态：边界=plain、向量=纯正文，
	// 已按新规则构建，BuildIndex 幂等跳过）
	mdocMarks := buildMirror(string(marksJSON), "?marks-ftsonly-eval=1", "AgenticSE Book CN (marks-ftsonly)")

	// 块边界等价不变式：正文同一份 → chunk 数必须一致
	stateP, _ := st.LoadDocumentIndexState(ctx, app, user, mdocPlain.ID)
	stateM, _ := st.LoadDocumentIndexState(ctx, app, user, mdocMarks.ID)
	if stateP.ChunkCount != stateM.ChunkCount {
		t.Fatalf("chunk 数漂移：plain %d vs marks %d（等价不变式被破坏）",
			stateP.ChunkCount, stateM.ChunkCount)
	}
	// 面包屑覆盖 ≥99%（验收门槛）
	chunks, err := st.LoadDocumentChunks(ctx, app, user, mdocMarks.ID)
	if err != nil {
		t.Fatalf("load chunks: %v", err)
	}
	covered := 0
	for _, c := range chunks {
		if strings.TrimSpace(c.SectionPath) != "" {
			covered++
		}
	}
	cover := 0.0
	if len(chunks) > 0 {
		cover = float64(covered) / float64(len(chunks))
	}
	if cover < 0.99 {
		t.Fatalf("面包屑覆盖 %.2f%% < 99%%（%d/%d）", cover*100, covered, len(chunks))
	}
	t.Logf("等价不变式通过：chunk 数 %d，面包屑覆盖 %.1f%%（%d/%d）",
		stateM.ChunkCount, cover*100, covered, len(chunks))

	// 同口径 48 题评分：plain 与 marks 同批运行（配对比较，消除运行间方差）
	r5p, r10p, mrrp := scoreSynonymCases(ctx, t, st, app, user, mdocPlain.ID, hybrid, emb, reranker)
	r5m, r10m, mrrm := scoreSynonymCases(ctx, t, st, app, user, mdocMarks.ID, hybrid, emb, reranker)
	t.Logf("==== F3c marks-FTS-only A/B（冻结 48 题，%s）====", map[bool]string{true: "云混合", false: "词法"}[hybrid])
	t.Logf("plain：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", r5p*100, r10p*100, mrrp)
	t.Logf("marks：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", r5m*100, r10m*100, mrrm)
	t.Logf("Δ    ：Recall@5=%+.0fpp  Recall@10=%+.0fpp  MRR=%+.2f",
		(r5m-r5p)*100, (r10m-r10p)*100, mrrm-mrrp)
	t.Logf("预注册切换线：marks 两轮均值 Recall@5 ≥ 52%% 且 MRR ≥ 0.41 且 Δ ≥ -2pp")
}

// markdownHeadingLines 统计 markdown 标题行数。
func markdownHeadingLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			out = append(out, t)
		}
	}
	return out
}
