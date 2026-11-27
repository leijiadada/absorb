package document_test

// f11_lexseed_eval_test.go —— F11（2026-11-20）：精排池补充词法种子两档对照
// （N=0 生产腿 / N=25 / N=50）。预注册全文见 design/5.md「F11 预注册」——
// 先写后跑，规则摘要：
//
//   - 单变量：只动精排池词法种子数；融合榜（RRF 吃双路 top50）、向量种子 40、
//     精排 query、精排模型（生产现款 8B，F9 形态）均不动；三腿同 run 配对
//   - 机制判据（主）：C 桶（词法可见 + 融合>16 + 向量种子外，词法盲区桶审计
//     存档 2026-11-20，先验 C=5）获救 ≥2（两轮一致）；=0 → 假设证伪关闭
//   - 转正候选线（沿 F4c 口径从严）：两轮均值 Δ R@5 ≥ +4pp 且生产腿命中题
//     零丢失 且对照 4/4 且 Book2 零回退（Δ R@5 ≥ -4pp）→ 接线评估；
//     任一不满足 → F11 关闭，池维持 F4b 形态
//   - 零丢失硬门继承 F4c 裁决契约；丢失逐题归因留档，不得以净增益对冲
//
// 范围口径：Book1 = 用户全部已索引文档（与词法盲区桶审计同口径，C 桶先验
// 直接可比）；Book2 = 单文档隔离（F4c 口径）。
//
// 运行（每轮一次；需 ABSORB_CLOUD_EMBED=1，key 取 ABSORB_EMB_API_KEY 或回退
// 应用配置 config.yaml；ABSORB_F11_ROUND 标注轮次仅用于日志）：
//
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_RR_MODEL="Qwen/Qwen3-Reranker-8B"
//	go test ./internal/document/ -run TestF11AgenticSE -count=1 -v
//	go test ./internal/document/ -run TestF11Book2 -count=1 -v

import (
	"context"
	"encoding/json"
	"fmt"
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

// f11Env 两书共用的评测环境（与 f4cEnv 同构）。
type f11Env struct {
	st       storage.Storage
	app      string
	user     string
	docID    string
	emb      retrieval.Embedder
	reranker retrieval.Reranker
	vhits    []storage.DocumentChunkHit
	vrows    []storage.ChunkVectorRow
}

// f11CaseResult 单题三腿结果。
type f11CaseResult struct {
	name      string
	lexRank   int  // 词法 top50 内锚点名次（0=未召回）
	vecRank   int  // 向量 top50 内锚点名次
	fusedRank int  // 融合榜内锚点名次
	cBucket   bool // 词法盲区桶：词法可见+融合>16+向量种子外（审计口径）
	rankN0    int
	rankN25   int
	rankN50   int
	poolN25   int
	poolN50   int
	msN0      int64
	msN25     int64
	msN50     int64
	fbN0      bool
	fbN25     bool
	fbN50     bool
}

// f11Pool 精排池组装：融合 top16 ∪ 向量 top40 ∪ 词法 topN（去重，融合序→
// 向量序→词法序）。lexN=0 退化为 F4b 生产形态。
func f11Pool(fused, vecTop, lexTop []storage.DocumentChunkHit, lexN int) ([]storage.DocumentChunkHit, map[string]struct{}) {
	head := fused
	if len(head) > rrTopN {
		head = head[:rrTopN]
	}
	pool := make([]storage.DocumentChunkHit, 0, len(head)+len(vecTop)+lexN)
	keys := make(map[string]struct{}, len(head)+len(vecTop)+lexN)
	add := func(list []storage.DocumentChunkHit) {
		for _, h := range list {
			k := chunkKeyOf(h)
			if _, ok := keys[k]; ok {
				continue
			}
			keys[k] = struct{}{}
			pool = append(pool, h)
		}
	}
	add(head)
	add(vecTop)
	if lexN > 0 && len(lexTop) > 0 {
		lex := lexTop
		if len(lex) > lexN {
			lex = lex[:lexN]
		}
		add(lex)
	}
	return pool, keys
}

// f11RunCase 单题三腿：共享词法/向量/融合榜，只差词法种子数。
// 词法检索/查询向量化失败直接 Fatal：云抖动记成 rank=0 会污染零丢失判定
// （与 f4cRunCase 同类错误处理一致）。
func f11RunCase(t *testing.T, ctx context.Context, env *f11Env, name, query, anchor string) f11CaseResult {
	t.Helper()
	a := document.SharedAnalyzer()
	lex50, err := env.st.SearchDocumentChunks(ctx, env.app, env.user, a.QueryExpression(query), env.docScope(), 50)
	if err != nil {
		t.Fatalf("f11 lex search failed (case=%s): %v", name, err)
	}
	qv, qerr := env.emb.EmbedQuery(ctx, query)
	if qerr != nil {
		t.Fatalf("f11 embed query failed (case=%s): %v", name, qerr)
	}
	vec50 := retrieval.TopKByCosine(env.vhits, env.vrows, qv, 50)
	vec40 := vec50
	if len(vec50) > 40 {
		vec40 = vec50[:40]
	}
	fused := retrieval.FuseRRF(lex50, vec50, 50)

	out := f11CaseResult{
		name:      name,
		lexRank:   bestRank(lex50, anchor),
		vecRank:   bestRank(vec50, anchor),
		fusedRank: bestRank(fused, anchor),
	}
	out.cBucket = out.lexRank >= 1 &&
		!(out.fusedRank >= 1 && out.fusedRank <= rrTopN) &&
		!(out.vecRank >= 1 && out.vecRank <= 40)

	runLeg := func(pool []storage.DocumentChunkHit, keys map[string]struct{}) (int, int64, bool) {
		t0 := time.Now()
		rr, applied := retrieval.RerankCandidates(ctx, pool, query, env.reranker, len(pool))
		ms := time.Since(t0).Milliseconds()
		// 注意：出参第三位 = 是否降级（!applied），与 f4cRunCase 的 fallbackB 语义一致
		return f4cRankOf(rr, fused[rrTopN:], keys, anchor), ms, !applied
	}
	pool0, keys0 := f11Pool(fused, vec40, nil, 0)
	out.rankN0, out.msN0, out.fbN0 = runLeg(pool0, keys0)
	pool25, keys25 := f11Pool(fused, vec40, lex50, 25)
	out.poolN25 = len(pool25)
	out.rankN25, out.msN25, out.fbN25 = runLeg(pool25, keys25)
	pool50, keys50 := f11Pool(fused, vec40, lex50, 50)
	out.poolN50 = len(pool50)
	out.rankN50, out.msN50, out.fbN50 = runLeg(pool50, keys50)
	return out
}

// docScope 检索范围：docID 为空 = 用户全部已索引文档（Book1 审计口径）。
func (e *f11Env) docScope() []string {
	if e.docID == "" {
		return nil
	}
	return []string{e.docID}
}

// f11RunSuite 三腿主循环 + 汇总（两书共用）。汇总输出按预注册判定线逐项对照。
func f11RunSuite(t *testing.T, label string, env *f11Env, cases []book2Case) {
	t.Helper()
	var results []f11CaseResult
	t.Logf("%s | %d 题 | 三腿 = 词法种子 0 / 25 / 50（池 = 融合 top%d ∪ 向量 top40 ∪ 词法 topN）",
		label, len(cases), rrTopN)
	t.Logf("%-28s %4s %4s %5s %6s %6s %6s %s", "题目", "词法", "向量", "融合", "N=0", "N=25", "N=50", "池25/50")
	for _, c := range cases {
		r := f11RunCase(t, context.Background(), env, c.name, c.query, c.anchor)
		results = append(results, r)
		t.Logf("%-28s %4d %4d %5d %6s %6s %6s %d/%d",
			r.name, r.lexRank, r.vecRank, r.fusedRank,
			rankLabel(r.rankN0), rankLabel(r.rankN25), rankLabel(r.rankN50),
			r.poolN25, r.poolN50)
	}

	type legStat struct {
		h5, h10       float64
		mrr           float64
		lost          int
		lostNames     []string
		rescuedC      int
		rescuedCNames []string
		ctlMiss       int
		appliedCnt    int
		fallback      int
		msSum         int64
		poolSum       int64
	}
	legs := map[string]*legStat{"N0": {}, "N25": {}, "N50": {}}
	getRank := func(r f11CaseResult, leg string) int {
		switch leg {
		case "N25":
			return r.rankN25
		case "N50":
			return r.rankN50
		}
		return r.rankN0
	}
	getLost := func(r f11CaseResult, leg string) bool { // 生产腿命中但该腿丢失
		return r.rankN0 > 0 && getRank(r, leg) == 0
	}
	var synN float64
	var cBucketN int
	for _, r := range results {
		isSyn := !strings.HasPrefix(r.name, "对照")
		for _, leg := range []string{"N0", "N25", "N50"} {
			s := legs[leg]
			fb := r.fbN0
			if leg == "N25" {
				fb = r.fbN25
			} else if leg == "N50" {
				fb = r.fbN50
			}
			if fb {
				s.fallback++
				t.Logf("  [warn] %s %s 腿精排降级融合序——该题该腿口径失真", r.name, leg)
			} else {
				s.appliedCnt++
			}
			rank := getRank(r, leg)
			if leg == "N25" {
				s.poolSum += int64(r.poolN25)
			} else if leg == "N50" {
				s.poolSum += int64(r.poolN50)
			}
			s.msSum += map[string]int64{"N0": r.msN0, "N25": r.msN25, "N50": r.msN50}[leg]
			if !isSyn {
				if rank == 0 {
					s.ctlMiss++
				}
				continue
			}
			if rank > 0 {
				s.h10++
				s.mrr += 1 / float64(rank)
				if rank <= 5 {
					s.h5++
				}
			}
			if leg != "N0" && getLost(r, leg) {
				s.lost++
				s.lostNames = append(s.lostNames, fmt.Sprintf("%s(N0=%d)", r.name, r.rankN0))
			}
		}
		if isSyn {
			synN++
			if r.cBucket {
				cBucketN++
				if r.rankN25 > 0 {
					legs["N25"].rescuedC++
					legs["N25"].rescuedCNames = append(legs["N25"].rescuedCNames,
						fmt.Sprintf("%s(→%d)", r.name, r.rankN25))
				}
				if r.rankN50 > 0 {
					legs["N50"].rescuedC++
					legs["N50"].rescuedCNames = append(legs["N50"].rescuedCNames,
						fmt.Sprintf("%s(→%d)", r.name, r.rankN50))
				}
			}
		}
	}

	n := synN
	n0 := legs["N0"]
	t.Logf("")
	t.Logf("==== F11 %s（%d 同义题，C 桶先验 %d）====", label, int(n), cBucketN)
	t.Logf("N=0（生产形态） ：R@5=%.0f%%  R@10=%.0f%%  MRR=%.2f", n0.h5/n*100, n0.h10/n*100, n0.mrr/n)
	for _, leg := range []string{"N25", "N50"} {
		s := legs[leg]
		t.Logf("%s：R@5=%.0f%%  R@10=%.0f%%  MRR=%.2f ｜ Δ R@5=%+.1fpp  Δ R@10=%+.1fpp  Δ MRR=%+.2f",
			leg, s.h5/n*100, s.h10/n*100, s.mrr/n,
			(s.h5-n0.h5)/n*100, (s.h10-n0.h10)/n*100, (s.mrr-n0.mrr)/n)
		t.Logf("  零丢失：生产腿命中丢失 %d %v", s.lost, s.lostNames)
		t.Logf("  C 桶获救：%d/%d %v", s.rescuedC, cBucketN, s.rescuedCNames)
		t.Logf("  对照组 miss=%d  fallback=%d  池均 %.0f 条  延迟均 %dms（×%.1f vs N=0）",
			s.ctlMiss, s.fallback, float64(s.poolSum)/n, s.msSum/int64(len(results)),
			float64(s.msSum)/float64(n0.msSum))
	}
	t.Logf("N=0 对照 miss=%d  fallback=%d", n0.ctlMiss, n0.fallback)
}

func TestF11AgenticSE(t *testing.T) {
	env := f11SetupCloud(t)
	ctx := context.Background()

	const bookURL = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"
	dc, err := env.st.LoadDocumentContent(ctx, env.app, env.user, bookURL)
	if err != nil || dc == nil {
		t.Skipf("副本中无目标文档正文 (err=%v)", err)
	}
	doc, err := env.st.UpsertDocument(ctx, env.app, env.user, bookURL, "AgenticSE Book CN")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	document.NewService(env.st, nil, env.app, env.user).WithEmbedder(env.emb).BuildIndex(ctx, doc.ID, dc)
	if state, _ := env.st.LoadDocumentIndexState(ctx, env.app, env.user, doc.ID); state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Fatalf("index not ready: %+v", state)
	}
	vhits, vrows, err := env.st.LoadVectorHits(ctx, env.app, env.user, env.emb.Model(), nil)
	if err != nil || len(vrows) == 0 {
		t.Fatalf("向量未入库 (err=%v, rows=%d)", err, len(vrows))
	}
	env.docID = "" // 审计口径：用户全部已索引文档
	env.vhits, env.vrows = vhits, vrows
	round := os.Getenv("ABSORB_F11_ROUND")
	if round == "" {
		round = "1"
	}
	t.Logf("主书向量 rows=%d（%s，范围=全部文档，与审计同口径）", len(vrows), env.emb.Model())

	for _, c := range synonymCases {
		if !strings.Contains(dc.ContentText, c.anchor) {
			t.Fatalf("锚点 %q 不在原文中（题目 %s）", c.anchor, c.name)
		}
	}
	f11RunSuite(t, fmt.Sprintf("Book1 AgenticSE 第%s轮", round), env, synonymCasesToBook2(synonymCases))
}

func TestF11Book2(t *testing.T) {
	env := f11SetupCloud(t)
	ctx := context.Background()

	// 自包含提取（与 TestF4CProbeBook2 同款）：本地 PDF → 本地 HTTP → 生产提取器
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
		ExtractionVersion: 5,
		FetchedAt:         time.Now(),
	}
	if marksJSON, merr := json.Marshal(res.Marks); merr == nil && len(res.Marks) > 0 {
		dc.HeadingMarks = string(marksJSON)
	}
	doc, err := env.st.UpsertDocument(ctx, env.app, env.user, book2URL, "人月神话（文字版）")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := env.st.SaveDocumentContent(ctx, env.app, env.user, dc); err != nil {
		t.Fatalf("save content: %v", err)
	}
	document.NewService(env.st, nil, env.app, env.user).WithEmbedder(env.emb).BuildIndex(ctx, doc.ID, dc)
	vhits, vrows, err := env.st.LoadVectorHits(ctx, env.app, env.user, env.emb.Model(), []string{doc.ID})
	if err != nil || len(vrows) == 0 {
		t.Fatalf("向量未入库 (err=%v, rows=%d)", err, len(vrows))
	}
	env.docID, env.vhits, env.vrows = doc.ID, vhits, vrows
	t.Logf("Book2：%d 向量（单文档隔离，F4c 口径）", len(vrows))

	round := os.Getenv("ABSORB_F11_ROUND")
	if round == "" {
		round = "1"
	}
	f11RunSuite(t, fmt.Sprintf("Book2 人月神话 第%s轮", round), env, book2Cases)
}

// f11SetupCloud 云环境装配：key 取环境变量，缺省回退应用配置 config.yaml
// （与词法盲区桶审计同源）；精排模型默认生产现款 8B（F9 形态）。
func f11SetupCloud(t *testing.T) *f11Env {
	t.Helper()
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F11 判定口径 = 云混合；设置 ABSORB_CLOUD_EMBED=1 后运行")
	}
	embKey := os.Getenv("ABSORB_EMB_API_KEY")
	keySrc := "env"
	if embKey == "" {
		embKey = loadEmbKeyFromConfig()
		keySrc = "config.yaml"
	}
	if embKey == "" {
		t.Fatal("cloud 模式需要 ABSORB_EMB_API_KEY 或应用配置 embedding.api_key")
	}
	embModel := os.Getenv("ABSORB_EMB_MODEL")
	if embModel == "" {
		embModel = "Qwen/Qwen3-Embedding-4B"
	}
	rrModel := os.Getenv("ABSORB_RR_MODEL")
	if rrModel == "" {
		rrModel = "Qwen/Qwen3-Reranker-8B" // F9 转正形态（生产现款）
	}
	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	retrieval.SetSharedEmbedder(emb)
	reranker := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
	t.Logf("F11 云环境：emb=%s  rr=%s（key 来源=%s）", embModel, rrModel, keySrc)
	return &f11Env{
		st: st, app: "absorb-reader", user: "absorb-user",
		emb: emb, reranker: reranker,
	}
}
