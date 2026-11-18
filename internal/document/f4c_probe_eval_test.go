package document_test

// f4c_probe_eval_test.go —— F4c 探针（2026-11-18）：精排池向量种子 40→80，
// 两书并行（AgenticSE 48 题主判定 + 人月神话 15 题泛化观测）。
// 预注册全文见 design/5.md「F4c 探针预注册」——先写后跑，规则摘要：
//
//   - 机制判据（主）：F4b 腿 miss 题中锚点在向量 top80 的题数 = 可救候选 R；
//     R=0 → 语料感知改写核心前提证伪，F4c 一并关闭
//   - 转正候选线：两轮均值 主书 Δ R@5 ≥ +4pp 且 F4b 命中题零丢失且对照 4/4
//     且 Book2 零回退（Δ R@5 ≥ -4pp）→ 接线评估；任一不满足 → F4c 关闭
//   - 单变量：只动精排池种子；融合榜（RRF 吃 vec50）/精排 query/词法腿不动，
//     两腿同 run 配对（共享融合榜）
//
// 运行（每轮一次，需 ABSORB_CLOUD_EMBED=1 + ABSORB_EMB_API_KEY）：
//
//	go test ./internal/document/ -run TestF4CProbeAgenticSE -count=1 -v
//	go test ./internal/document/ -run TestF4CProbeBook2   -count=1 -v

import (
	"context"
	"encoding/json"
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
	searchdoc "absorb-agent/tool/search_document"
)

// f4cEnv 两书共用的探针环境：已就绪的文档索引 + 向量 + 精排客户端。
type f4cEnv struct {
	st       storage.Storage
	app      string
	user     string
	docID    string
	emb      retrieval.Embedder
	reranker retrieval.Reranker
	vhits    []storage.DocumentChunkHit
	vrows    []storage.ChunkVectorRow
}

// f4cCaseResult 单题两腿结果。
type f4cCaseResult struct {
	name      string
	rankF4b   int
	rankF4c   int
	vecRank80 int // 锚点在向量 top80 的排名（0=不在——机制判据核心字段）
	poolN     int
	msF4b     int64
	msF4c     int64
	fallbackB bool
	fallbackC bool
}

// f4cRunCase 单题两腿：共享词法/向量/融合榜，只差精排池向量种子（40 vs 80）。
func f4cRunCase(ctx context.Context, env *f4cEnv, name, query, anchor string) f4cCaseResult {
	out := f4cCaseResult{name: name}
	a := document.SharedAnalyzer()

	lex, err := env.st.SearchDocumentChunks(ctx, env.app, env.user, a.QueryExpression(query), []string{env.docID}, 50)
	if err != nil {
		return out
	}
	qv, qerr := env.emb.EmbedQuery(ctx, query)
	if qerr != nil {
		return out
	}
	vec50 := retrieval.TopKByCosine(env.vhits, env.vrows, qv, 50)
	vec80 := retrieval.TopKByCosine(env.vhits, env.vrows, qv, 80)
	vec40 := vec50
	if len(vec50) > 40 {
		vec40 = vec50[:40]
	}
	fused := retrieval.FuseRRF(lex, vec50, 50)

	// 机制判据字段：锚点在向量 top80 的排名
	for i, h := range vec80 {
		if strings.Contains(h.Content, anchor) {
			out.vecRank80 = i + 1
			break
		}
	}

	// F4b 腿（生产形态）：池 = 融合 top16 ∪ 向量 top40
	poolB, keysB := rerankPoolF4B(fused, vec40)
	t0 := time.Now()
	rrB, appliedB := retrieval.RerankCandidates(ctx, poolB, query, env.reranker, len(poolB))
	out.msF4b = time.Since(t0).Milliseconds()
	out.fallbackB = !appliedB
	out.rankF4b = f4cRankOf(rrB, fused[rrTopN:], keysB, anchor)

	// F4c 腿：池 = 融合 top16 ∪ 向量 top80
	poolC, keysC := rerankPoolF4B(fused, vec80)
	out.poolN = len(poolC)
	t1 := time.Now()
	rrC, appliedC := retrieval.RerankCandidates(ctx, poolC, query, env.reranker, len(poolC))
	out.msF4c = time.Since(t1).Milliseconds()
	out.fallbackC = !appliedC
	out.rankF4c = f4cRankOf(rrC, fused[rrTopN:], keysC, anchor)
	return out
}

// f4cRankOf 精排结果 + 池外融合尾缀后 → top10 内锚点排名（0=miss）。
func f4cRankOf(rr, fusedTail []storage.DocumentChunkHit, poolKeys map[string]struct{}, anchor string) int {
	var tail []storage.DocumentChunkHit
	for _, h := range fusedTail {
		if _, ok := poolKeys[chunkKeyOf(h)]; !ok {
			tail = append(tail, h)
		}
	}
	final, _ := searchdoc.DeduplicateHits(append(rr, tail...), 10)
	for i, h := range final {
		if i < 10 && strings.Contains(h.Content, anchor) {
			return i + 1
		}
	}
	return 0
}

// f4cRunSuite 探针主循环 + 汇总（两书共用）。
func f4cRunSuite(t *testing.T, label string, env *f4cEnv, cases []book2Case) {
	t.Helper()
	var results []f4cCaseResult
	t.Logf("%s | %d 题 | 池 = 融合 top%d ∪ 向量 top40 vs top80", label, len(cases), rrTopN)
	t.Logf("%-20s %-10s %-10s %-8s %s", "题目", "F4b", "F4c", "vec80", "msB/msC")
	for _, c := range cases {
		r := f4cRunCase(context.Background(), env, c.name, c.query, c.anchor)
		results = append(results, r)
		t.Logf("%-20s %-10s %-10s %-8d %d/%d",
			r.name, rankLabel(r.rankF4b), rankLabel(r.rankF4c), r.vecRank80, r.msF4b, r.msF4c)
	}

	var (
		b5, b10, mrrB, c5, c10, mrrC, synN       float64
		lost, rescuable, unrescuable             int
		ctlMissB, ctlMissC, fallbackB, fallbackC int
		msB, msC, poolSum                        int64
	)
	for _, r := range results {
		isSyn := !strings.HasPrefix(r.name, "对照")
		msB, msC, poolSum = msB+r.msF4b, msC+r.msF4c, poolSum+int64(r.poolN)
		if r.fallbackB {
			fallbackB++
			t.Logf("  [warn] %s F4b 腿精排降级融合序——该题该腿口径失真", r.name)
		}
		if r.fallbackC {
			fallbackC++
			t.Logf("  [warn] %s F4c 腿精排降级融合序——该题该腿口径失真", r.name)
		}
		if !isSyn {
			if r.rankF4b == 0 {
				ctlMissB++
			}
			if r.rankF4c == 0 {
				ctlMissC++
			}
			continue
		}
		synN++
		if r.rankF4b > 0 {
			mrrB += 1 / float64(r.rankF4b)
			b10++
			if r.rankF4b <= 5 {
				b5++
			}
		}
		if r.rankF4c > 0 {
			mrrC += 1 / float64(r.rankF4c)
			c10++
			if r.rankF4c <= 5 {
				c5++
			}
		} else if r.rankF4b > 0 {
			lost++ // F4b 命中 → F4c 丢失（转正线硬门槛）
		}
		if r.rankF4b == 0 {
			if r.vecRank80 > 0 {
				rescuable++
			} else {
				unrescuable++
			}
		}
	}
	n := synN
	t.Logf("")
	t.Logf("==== F4c 探针 %s（%d 同义题，单轮）====", label, int(n))
	t.Logf("F4b（种子40）：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR=%.2f", b5/n*100, b10/n*100, mrrB/n)
	t.Logf("F4c（种子80）：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR=%.2f", c5/n*100, c10/n*100, mrrC/n)
	t.Logf("Δ：R@5=%+.0fpp  R@10=%+.0fpp  MRR=%+.2f ｜ F4b命中丢失=%d", (c5-b5)/n*100, (c10-b10)/n*100, (mrrC-mrrB)/n, lost)
	t.Logf("机制判据：F4b miss 中锚点在 vec80 = %d（可救候选 R），不在 = %d", rescuable, unrescuable)
	t.Logf("对照组：F4b miss=%d  F4c miss=%d", ctlMissB, ctlMissC)
	t.Logf("延迟：F4b 均 %dms ｜ F4c 均 %dms（池均 %d 条，×%.1f）｜ fallback B/C = %d/%d",
		msB/int64(len(results)), msC/int64(len(results)), poolSum/int64(len(results)),
		float64(msC)/float64(msB), fallbackB, fallbackC)
}

func TestF4CProbeAgenticSE(t *testing.T) {
	env, embModel := f4cSetupCloud(t)
	if t.Failed() {
		return
	}
	st, app, user := env.st, env.app, env.user
	ctx := context.Background()

	mdoc, err := st.UpsertDocument(ctx, app, user, "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"+popplerMirrorSuffix, "AgenticSE Book CN (poppler)")
	if err != nil {
		t.Fatalf("upsert mirror: %v", err)
	}
	state, _ := st.LoadDocumentIndexState(ctx, app, user, mdoc.ID)
	if state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Skipf("plain 镜像未就绪（先跑 TestSynonymRetrievalMarksAB 建镜像）: %+v", state)
	}
	vhits, vrows, err := st.LoadVectorHits(ctx, app, user, embModel, []string{mdoc.ID})
	if err != nil || len(vrows) == 0 {
		t.Skipf("镜像向量缺失 (err=%v)", err)
	}
	env.docID, env.vhits, env.vrows = mdoc.ID, vhits, vrows
	t.Logf("镜像：%d 向量（%s）", len(vrows), embModel)

	f4cRunSuite(t, "Book1 AgenticSE", env, synonymCasesToBook2(synonymCases))
}

func TestF4CProbeBook2(t *testing.T) {
	env, _ := f4cSetupCloud(t)
	if t.Failed() {
		return
	}
	st, app, user := env.st, env.app, env.user
	ctx := context.Background()

	// 自包含提取（与 book2_eval_test.go 同款）：本地 PDF → 本地 HTTP → 生产提取器
	bookPath := findRepoPathFrom("docs/pdf/人月神话-中文-目录-文字版.pdf")
	if bookPath == "" {
		t.Skip("样书不存在")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/book.pdf", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, bookPath)
	})
	go http.Serve(ln, mux)
	ex := extract.NewPDFExtractor(fetch.NewFetchPageService(func() string { return "" },
		func() int64 { return 200 << 20 }), func() string { return "" },
		func() int64 { return 200 << 20 })
	xctx, xcancel := context.WithTimeout(ctx, 5*time.Minute)
	defer xcancel()
	res, err := ex.Extract(xctx, "http://"+ln.Addr().String()+"/book.pdf")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	dc := &storage.DocumentContent{
		URL: book2URL, ContentText: res.Content,
		CharCount: len([]rune(res.Content)), ContentType: res.ContentType,
		ExtractionStatus:  extract.StatusReady,
		ExtractionVersion: 5, // bookkeeping 字段，BuildIndex 不读
		FetchedAt:         time.Now(),
	}
	if marksJSON, merr := json.Marshal(res.Marks); merr == nil && len(res.Marks) > 0 {
		dc.HeadingMarks = string(marksJSON)
	}
	doc, err := st.UpsertDocument(ctx, app, user, book2URL, "人月神话（文字版）")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.SaveDocumentContent(ctx, app, user, dc); err != nil {
		t.Fatalf("save content: %v", err)
	}
	document.NewService(st, nil, app, user).WithEmbedder(env.emb).BuildIndex(ctx, doc.ID, dc)
	vhits, vrows, err := st.LoadVectorHits(ctx, app, user, env.emb.Model(), []string{doc.ID})
	if err != nil || len(vrows) == 0 {
		t.Fatalf("向量未入库 (err=%v, rows=%d)", err, len(vrows))
	}
	env.docID, env.vhits, env.vrows = doc.ID, vhits, vrows
	t.Logf("Book2：%d 向量", len(vrows))

	f4cRunSuite(t, "Book2 人月神话", env, book2Cases)
}

// f4cSetupCloud 云环境装配（key/模型/精排客户端 + 库副本）。
func f4cSetupCloud(t *testing.T) (*f4cEnv, string) {
	t.Helper()
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F4c 判定口径 = 云混合；设置 ABSORB_CLOUD_EMBED=1 后运行")
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
	t.Cleanup(func() { st.Close() })
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	reranker := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
	return &f4cEnv{
		st: st, app: "absorb-reader", user: "absorb-user",
		emb: emb, reranker: reranker,
	}, embModel
}

// synonymCasesToBook2 主书 48 题（synonymCase）转探针通用用例形态。
func synonymCasesToBook2(cases []synonymCase) []book2Case {
	out := make([]book2Case, 0, len(cases))
	for _, c := range cases {
		out = append(out, book2Case{name: c.name, query: c.query, anchor: c.anchor})
	}
	return out
}
