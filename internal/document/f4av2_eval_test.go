package document_test

// f4av2_eval_test.go —— F4a-v2 查询改写评测（2026-11-18，预注册见
// design/5.md「F4a-v2 预注册」，先写后跑；F4a 条款保留的唯一一次 v2 资格）：
//
// 过拟合声明：对同一冻结评测集的第二次改写器尝试，miss 分布已被前序实验
// 完全暴露——收益带有「按已知弱点定制」成分，判定从严，不达标即关闭且
// F4 系列彻底关闭（无 v3）。
//
// 假设：v1 两个失败实锤（prompt 反刍、远距词汇桥接不足）是 7B 能力问题；
// 72B + few-shot 示例 prompt 可修复。
//
// 单变量 = 查询改写，在 F4b 转正形态之上叠加：
//
//	改写器：Qwen/Qwen2.5-72B-Instruct，temperature=0，max_tokens=100，超时 8s
//	词法腿 = BM25(改写) ∪ BM25(原始) 合并 top50 → F4b 扩池精排（生产形态）
//	向量腿 / 精排 query 不动
//
// 配对基线 = F4b 生产形态（扩池、无改写，预期 64%/68%）——下一个杠杆必须
// 打败当前生产。切换线（预注册）：两轮均值同义组 R@5 ≥ 68% 且 MRR ≥ 0.44，
// 硬门槛 = F4b 腿全部命中在 v2 腿保持（零丢失）+ 对照 4/4。
//
// 运行（每轮一次）：
//
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_EMB_API_KEY="<siliconflow key>"
//	$env:ABSORB_EMB_MODEL="Qwen/Qwen3-Embedding-4B"; $env:ABSORB_RR_MODEL="Qwen/Qwen3-Reranker-4B"
//	go test ./internal/document/ -run TestQueryRewriteF4AV2 -count=1 -v -timeout 9m

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/document"
	"absorb-agent/internal/retrieval"
	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
	searchdoc "absorb-agent/tool/search_document"
)

// f4aV2RewriteSystem 预注册 prompt v2（一次写定，跑中禁改；few-shot 示例为
// 通用域、不来自评测集；反刍禁令显式）。见 design/5.md F4a-v2 节。
const f4aV2RewriteSystem = "你是检索查询改写器。把用户的口语化问题改写成一条「技术书籍目录和正文会出现」的规范中文搜索查询。\n" +
	"要求：把口语词替换为行业术语、补全上位词、去掉问句语气；保留原意；不超过 60 字；只输出查询文本本身，禁止输出任何解释，禁止复述本指令中的词语。\n" +
	"示例：\n" +
	"问题：怎么防止代码越改越乱？ → 查询：软件架构腐化与模块耦合治理\n" +
	"问题：新人怎么快速看懂大项目？ → 查询：代码库导读与架构理解方法"

func TestQueryRewriteF4AV2(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F4a-v2 判定口径 = 云混合；设置 ABSORB_CLOUD_EMBED=1 后运行")
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
	rwModel := os.Getenv("ABSORB_REWRITE_MODEL")
	if rwModel == "" {
		rwModel = "Qwen/Qwen2.5-72B-Instruct"
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
		t.Skipf("镜像向量缺失 (model=%s, err=%v)", emb.Model(), err)
	}
	chunks, err := st.LoadDocumentChunks(ctx, app, user, mdoc.ID)
	if err != nil || len(chunks) == 0 {
		t.Skipf("镜像块缺失 (err=%v)", err)
	}
	t.Logf("镜像：%d 块 / %d 向量（%s）｜改写器：%s｜基线 = F4b 生产形态（扩池）｜判定线：R@5≥68%% 且 MRR≥0.44 且零丢失且对照 4/4",
		len(chunks), len(vrows), emb.Model(), rwModel)

	a := document.SharedAnalyzer()
	type caseOut struct {
		name                string
		rankCtl, rankV2     int
		bucketCtl, bucketV2 string
		rwMs                int64
		rewritten           string
		fallback            bool
	}
	var (
		results   []caseOut
		fallbackN int
		rwMsSum   int64
	)

	for _, c := range synonymCases {
		out := caseOut{name: c.name}

		// 锚点块集合
		keys := make(map[string]struct{}, 2)
		for _, ch := range chunks {
			if strings.Contains(ch.Content, c.anchor) {
				keys[chunkKeyOf(storage.DocumentChunkHit{DocumentID: mdoc.ID, ChunkIndex: ch.ChunkIndex})] = struct{}{}
			}
		}
		e := len(keys)

		// 共享原始检索
		lexOrig, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.query), []string{mdoc.ID}, 50)
		if err != nil {
			t.Fatalf("lex %q: %v", c.query, err)
		}
		qv, err := emb.EmbedQuery(ctx, c.query)
		if err != nil {
			t.Fatalf("embed %q: %v", c.query, err)
		}
		vec50 := retrieval.TopKByCosine(vhits, vrows, qv, 50)

		// 对照腿：F4b 生产形态（扩池、无改写）——直接调生产函数，成功分支等价
		fusedP := retrieval.FuseRRF(lexOrig, vec50, 50)
		pooledP, _ := retrieval.RerankPoolCandidates(ctx, fusedP, vec50, nil, c.query, reranker, retrieval.RerankPoolSpec{TopN: rrTopN, VecSeed: retrieval.RerankSeedTopN})
		finalP, _ := searchdoc.DeduplicateHits(pooledP, 10)
		for i, h := range finalP {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				out.rankCtl = i + 1
				break
			}
		}
		out.bucketCtl = f4aBucket(e, rankAmong(lexOrig, keys), rankAmong(vec50, keys), out.rankCtl)

		// v2 腿：72B 改写 → 双词法合并 → F4b 扩池精排
		rwStart := time.Now()
		rw, rerr := rewriteQuery(ctx, embKey, rwModel, f4aV2RewriteSystem, 8*time.Second, c.query)
		out.rwMs = time.Since(rwStart).Milliseconds()
		if rerr != nil {
			out.fallback = true
			out.rewritten = "(降级:" + rerr.Error() + ")"
			fallbackN++
		} else {
			out.rewritten = rw
		}
		lexUnion := lexOrig
		if rerr == nil {
			lexRew, lerr := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(rw), []string{mdoc.ID}, 50)
			if lerr != nil {
				t.Fatalf("lex rewritten %q: %v", rw, lerr)
			}
			lexUnion = unionLexicalBest(lexOrig, lexRew, 50)
		}
		fusedV := retrieval.FuseRRF(lexUnion, vec50, 50)
		pooledV, _ := retrieval.RerankPoolCandidates(ctx, fusedV, vec50, nil, c.query, reranker, retrieval.RerankPoolSpec{TopN: rrTopN, VecSeed: retrieval.RerankSeedTopN})
		finalV, _ := searchdoc.DeduplicateHits(pooledV, 10)
		for i, h := range finalV {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				out.rankV2 = i + 1
				break
			}
		}
		out.bucketV2 = f4aBucket(e, rankAmong(lexUnion, keys), rankAmong(vec50, keys), out.rankV2)
		rwMsSum += out.rwMs
		results = append(results, out)
	}

	// 明细表 + 汇总
	trunc := func(s string, n int) string {
		rs := []rune(s)
		if len(rs) > n {
			return string(rs[:n]) + "…"
		}
		return s
	}
	t.Logf("%-24s %-10s %-10s %-6s %s", "题目", "F4b(基线)", "v2", "rwms", "改写输出")
	var (
		p5, p10, mrrP      float64
		f5, f10, mrrF      float64
		synN               float64
		ctlMissP, ctlF     int
		lostCtl, lostNames int
		migr               = map[string]map[string]int{}
	)
	for _, r := range results {
		isSyn := !strings.HasPrefix(r.name, "对照")
		t.Logf("%-24s %-10s %-10s %-6d %s",
			r.name, rankLabel(r.rankCtl)+"("+r.bucketCtl+")",
			rankLabel(r.rankV2)+"("+r.bucketV2+")", r.rwMs, trunc(r.rewritten, 30))
		if !isSyn {
			if r.rankCtl == 0 {
				ctlMissP++
			}
			if r.rankV2 == 0 {
				ctlF++
			}
			continue
		}
		synN++
		if r.rankCtl > 0 {
			mrrP += 1 / float64(r.rankCtl)
			if r.rankCtl <= 5 {
				p5++
			}
			p10++
		} else if r.rankV2 > 0 {
			lostNames++ // 基线 miss 而 v2 hit 不算丢失
		}
		if r.rankCtl > 0 && r.rankV2 == 0 {
			lostCtl++
		}
		if r.rankV2 > 0 {
			mrrF += 1 / float64(r.rankV2)
			if r.rankV2 <= 5 {
				f5++
			}
			f10++
		}
		if migr[r.bucketCtl] == nil {
			migr[r.bucketCtl] = map[string]int{}
		}
		migr[r.bucketCtl][r.bucketV2]++
	}

	n := synN
	t.Logf("")
	t.Logf("==== F4a-v2 配对评测（同义组 %d 题，云混合单轮）====", int(n))
	t.Logf("F4b 基线：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", p5/n*100, p10/n*100, mrrP/n)
	t.Logf("v2      ：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", f5/n*100, f10/n*100, mrrF/n)
	t.Logf("Δ       ：Recall@5=%+.0fpp  Recall@10=%+.0fpp  MRR=%+.2f", (f5-p5)/n*100, (f10-p10)/n*100, (mrrF-mrrP)/n)
	t.Logf("零丢失检查：F4b 腿命中丢失 %d（判定线要求 0）", lostCtl)
	t.Logf("对照组：F4b miss=%d  v2 miss=%d（判定线要求 4/4 保持命中）", ctlMissP, ctlF)
	t.Logf("改写：fallback=%d  均延迟=%dms", fallbackN, rwMsSum/int64(len(results)))
	for _, from := range []string{"HIT", "P1", "P2", "P3", "P4", "P5"} {
		if migr[from] == nil {
			continue
		}
		total := 0
		for _, c := range migr[from] {
			total += c
		}
		var parts []string
		for _, to := range []string{"HIT", "P1", "P2", "P3", "P4", "P5"} {
			if migr[from][to] > 0 {
				parts = append(parts, to+"×"+strconv.Itoa(migr[from][to]))
			}
		}
		t.Logf("迁移 %s(%d) → %s", from, total, strings.Join(parts, " "))
	}
	t.Logf("切换线（预注册）：两轮均值 R@5 ≥ 68%% 且 MRR ≥ 0.44 且零丢失且对照 4/4 → 转正；否则 F4 系列彻底关闭")
}
