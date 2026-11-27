package document_test

// rerank_pool_eval_test.go —— F4b 精排候选池扩容评测（2026-11-18，预注册见
// design/5.md「F4b 预注册」，先写后跑）：
//
// 假设（miss autopsy 机制侧发现）：P3 桶 8 题向量路全在 top36 内但挤在融合
// 19~45 名，进不了精排 top16——「检索看得到，精排看不到」。精排池扩为
// 融合 top16 ∪ 向量 top40，让强精排直接判断。
//
// 单变量：只动精排候选池输入。RRF 融合榜不动（F4a 教训：union 逐块名次
// 不减≠融合名次不减——本方案不触碰融合榜，只动精排窗口）；词法腿/向量腿/
// 精排 query/去重全部不动；池外融合尾保持原序缀后。
//
// 单次调用 = 一轮配对评测（plain 腿 = 现管线 top16，F4b 腿 = 扩池，两腿
// 共享同一融合榜），两轮云混合取均值判定。切换线：两轮均值同义组 R@5 ≥
// 65% 且 MRR ≥ 0.44 且对照组 4/4 → 转正；任一不达标关闭 F4b、F4 系列关闭。
//
// 运行（每轮一次）：
//
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_EMB_API_KEY="<siliconflow key>"
//	$env:ABSORB_EMB_MODEL="Qwen/Qwen3-Embedding-4B"; $env:ABSORB_RR_MODEL="Qwen/Qwen3-Reranker-4B"
//	go test ./internal/document/ -run TestRerankPoolF4B -count=1 -v

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

// rerankPoolF4B 精排池 = 融合 top16 ∪ 向量种子（去重、融合序在前向量序在后）。
// 返回池与池成员键集（池外融合尾排他用）。
func rerankPoolF4B(fused, vecTop []storage.DocumentChunkHit) ([]storage.DocumentChunkHit, map[string]struct{}) {
	head := fused
	if len(head) > rrTopN {
		head = head[:rrTopN]
	}
	pool := make([]storage.DocumentChunkHit, 0, len(head)+len(vecTop))
	keys := make(map[string]struct{}, len(head)+len(vecTop))
	for _, h := range head {
		k := chunkKeyOf(h)
		if _, ok := keys[k]; !ok {
			keys[k] = struct{}{}
			pool = append(pool, h)
		}
	}
	for _, h := range vecTop {
		k := chunkKeyOf(h)
		if _, ok := keys[k]; !ok {
			keys[k] = struct{}{}
			pool = append(pool, h)
		}
	}
	return pool, keys
}

func TestRerankPoolF4B(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F4b 判定口径 = 云混合；设置 ABSORB_CLOUD_EMBED=1 后运行")
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
		t.Skipf("镜像向量缺失 (model=%s, err=%v)", emb.Model(), err)
	}
	chunks, err := st.LoadDocumentChunks(ctx, app, user, mdoc.ID)
	if err != nil || len(chunks) == 0 {
		t.Skipf("镜像块缺失 (err=%v)", err)
	}
	t.Logf("镜像：%d 块 / %d 向量（%s）｜池 = 融合 top%d ∪ 向量 top40｜判定线：R@5≥65%% 且 MRR≥0.44 且对照 4/4",
		len(chunks), len(vrows), emb.Model(), rrTopN)

	a := document.SharedAnalyzer()
	type caseOut struct {
		name                  string
		rankPlain, rankF4b    int
		bucketPlain, bucketF4 string
		ms16, msPool          int64
		poolN                 int
	}
	var results []caseOut

	for _, c := range synonymCases {
		out := caseOut{name: c.name}

		// 锚点块集合（桶归类用，滑窗切块下 0/1/多皆可能）
		keys := make(map[string]struct{}, 2)
		for _, ch := range chunks {
			if strings.Contains(ch.Content, c.anchor) {
				keys[chunkKeyOf(storage.DocumentChunkHit{DocumentID: mdoc.ID, ChunkIndex: ch.ChunkIndex})] = struct{}{}
			}
		}
		e := len(keys)

		// 共享原始检索（两腿同源）
		lexOrig, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.query), []string{mdoc.ID}, 50)
		if err != nil {
			t.Fatalf("lex %q: %v", c.query, err)
		}
		qv, err := emb.EmbedQuery(ctx, c.query)
		if err != nil {
			t.Fatalf("embed %q: %v", c.query, err)
		}
		vec50 := retrieval.TopKByCosine(vhits, vrows, qv, 50)
		vec40 := vec50
		if len(vec50) > 40 {
			vec40 = vec50[:40] // TopKByCosine 部分选择排序保证前缀即 top40
		}
		fused := retrieval.FuseRRF(lexOrig, vec50, 50) // 两腿共享同一融合榜（不触碰）

		// plain 腿：现管线（精排只看融合 top16）
		t0 := time.Now()
		rrP, _ := retrieval.RerankCandidates(ctx, fused, c.query, reranker, rrTopN)
		out.ms16 = time.Since(t0).Milliseconds()
		finalP, _ := searchdoc.DeduplicateHits(rrP, 10)
		for i, h := range finalP {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				out.rankPlain = i + 1
				break
			}
		}
		out.bucketPlain = f4aBucket(e, rankAmong(lexOrig, keys), rankAmong(vec50, keys), out.rankPlain)

		// F4b 腿：池 = 融合 top16 ∪ 向量 top40，整池精排，池外融合尾原序缀后
		pool, poolKeys := rerankPoolF4B(fused, vec40)
		out.poolN = len(pool)
		t1 := time.Now()
		rrB, _ := retrieval.RerankCandidates(ctx, pool, c.query, reranker, len(pool))
		out.msPool = time.Since(t1).Milliseconds()
		var tail []storage.DocumentChunkHit
		for _, h := range fused[rrTopN:] {
			if _, ok := poolKeys[chunkKeyOf(h)]; !ok {
				tail = append(tail, h)
			}
		}
		finalB, _ := searchdoc.DeduplicateHits(append(rrB, tail...), 10)
		for i, h := range finalB {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				out.rankF4b = i + 1
				break
			}
		}
		out.bucketF4 = f4aBucket(e, rankAmong(lexOrig, keys), rankAmong(vec50, keys), out.rankF4b)
		results = append(results, out)
	}

	// 明细表 + 汇总
	t.Logf("%-24s %-12s %-12s %-7s %s", "题目", "plain", "F4b", "ms16/ms", "池大小")
	var (
		p5, p10, mrrP      float64
		f5, f10, mrrF      float64
		synN               float64
		ctlMissP, ctlF     int
		ms16Sum, msPoolSum int64
		poolSum            int
		migr               = map[string]map[string]int{}
	)
	for _, r := range results {
		isSyn := !strings.HasPrefix(r.name, "对照")
		t.Logf("%-24s %-12s %-12s %-7d %d",
			r.name,
			rankLabel(r.rankPlain)+"("+r.bucketPlain+")",
			rankLabel(r.rankF4b)+"("+r.bucketF4+")",
			r.ms16, r.poolN)
		ms16Sum += r.ms16
		msPoolSum += r.msPool
		poolSum += r.poolN
		if !isSyn {
			if r.rankPlain == 0 {
				ctlMissP++
			}
			if r.rankF4b == 0 {
				ctlF++
			}
			continue
		}
		synN++
		if r.rankPlain > 0 {
			mrrP += 1 / float64(r.rankPlain)
			if r.rankPlain <= 5 {
				p5++
			}
			p10++
		}
		if r.rankF4b > 0 {
			mrrF += 1 / float64(r.rankF4b)
			if r.rankF4b <= 5 {
				f5++
			}
			f10++
		}
		if migr[r.bucketPlain] == nil {
			migr[r.bucketPlain] = map[string]int{}
		}
		migr[r.bucketPlain][r.bucketF4]++
	}

	n := synN
	t.Logf("")
	t.Logf("==== F4b 配对评测（同义组 %d 题，云混合单轮）====", int(n))
	t.Logf("plain：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", p5/n*100, p10/n*100, mrrP/n)
	t.Logf("F4b  ：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", f5/n*100, f10/n*100, mrrF/n)
	t.Logf("Δ    ：Recall@5=%+.0fpp  Recall@10=%+.0fpp  MRR=%+.2f", (f5-p5)/n*100, (f10-p10)/n*100, (mrrF-mrrP)/n)
	t.Logf("对照组：plain miss=%d  F4b miss=%d（判定线要求 4/4 保持命中）", ctlMissP, ctlF)
	t.Logf("精排延迟：16 条 %dms ｜ 池均 %d 条 %dms（×%.1f）",
		ms16Sum/int64(len(results)), int64(poolSum)/int64(len(results)), msPoolSum/int64(len(results)),
		float64(msPoolSum)/float64(ms16Sum))
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
	t.Logf("切换线（预注册）：两轮均值 R@5 ≥ 65%% 且 MRR ≥ 0.44 且对照 4/4 → 转正")
}
