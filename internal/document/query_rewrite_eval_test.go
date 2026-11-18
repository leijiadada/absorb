package document_test

// query_rewrite_eval_test.go —— F4a 查询改写评测（2026-11-18，预注册见
// design/5.md「F4a 预注册」，先写后跑）：
//
// 假设（miss autopsy 直接推论）：P3 词汇不匹配（8/19）的死因是问句用词与块
// 内用词零 BM25 交集而语义可桥接。改写器把问句翻成书内规范术语喂词法腿：
//
//	词法腿 = BM25(改写) ∪ BM25(原始)，按块取最优名次合并取前 50（纯增量）
//	向量腿 / 精排 query / 分块 / 索引 一律不动（单变量纪律）
//
// 单次调用 = 一轮配对评测：同 run 内每题先跑 plain 腿（原始管线）再跑 F4a
// 腿，原始检索共享、精排各自调用——两轮云混合取均值判定。
//
// 切换线（预注册）：两轮均值同义组 Recall@5 ≥ 65% 且 MRR ≥ 0.44，硬门槛
// 对照组 4/4 保持命中。不过线关闭；prompt 只允许一次 v2 重预注册。
//
// 改写器：OpenAI 兼容 chat（SiliconFlow 同 key 直连），
// ABSORB_REWRITE_MODEL（默认 Qwen/Qwen2.5-7B-Instruct），temperature=0，
// max_tokens=100，超时 3s，失败/空输出降级原始词法（记 fallback）。
//
// 运行（每轮一次）：
//
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_EMB_API_KEY="<siliconflow key>"
//	$env:ABSORB_EMB_MODEL="Qwen/Qwen3-Embedding-4B"; $env:ABSORB_RR_MODEL="Qwen/Qwen3-Reranker-4B"
//	go test ./internal/document/ -run TestQueryRewriteF4A -count=1 -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// f4aRewriteSystem 预注册 prompt（一次写定，跑中禁改；见 design/5.md F4a 节）。
const f4aRewriteSystem = "把用户问题改写成 1 条中文搜索查询：使用技术书籍的规范术语（同义词、上位词、行业词），保留原意，不超过 60 字，只输出查询本身。"

// f4aRewrite 单题改写：OpenAI 兼容 chat 直连，失败返回 error 由调用方降级。
func f4aRewrite(ctx context.Context, apiKey, model, question string) (string, error) {
	return rewriteQuery(ctx, apiKey, model, f4aRewriteSystem, 3*time.Second, question)
}

// rewriteQuery 改写共享核心（v1/v2 同款链路，prompt 与超时为参数——v2 见
// f4av2_eval_test.go）。
func rewriteQuery(ctx context.Context, apiKey, model, system string, timeout time.Duration, question string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": question},
		},
		"temperature": 0,
		"max_tokens":  100,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		"https://api.siliconflow.cn/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("rewrite http %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("rewrite empty choices")
	}
	c := out.Choices[0].Message.Content
	if i := strings.Index(c, "</think>"); i >= 0 { // 防 thinking 泄漏（防御性）
		c = c[i+len("</think>"):]
	}
	if i := strings.IndexAny(c, "\r\n"); i >= 0 { // 只取第一行
		c = c[:i]
	}
	c = strings.TrimSpace(c)
	c = strings.Trim(c, "\"“”「」")
	rs := []rune(c)
	if len(rs) == 0 {
		return "", errors.New("rewrite empty")
	}
	if len(rs) > 120 {
		c = string(rs[:120])
	}
	return c, nil
}

// chunkKeyOf 块身份键（与 FuseRRF 同款：documentID#chunkIndex）。
func chunkKeyOf(h storage.DocumentChunkHit) string {
	return h.DocumentID + "#" + strconv.Itoa(h.ChunkIndex)
}

// rankAmong keys 中任意块在 hits 里的最优名次（1-based，0=不在列表）。
func rankAmong(hits []storage.DocumentChunkHit, keys map[string]struct{}) int {
	for i, h := range hits {
		if _, ok := keys[chunkKeyOf(h)]; ok {
			return i + 1
		}
	}
	return 0
}

// unionLexicalBest 双词法列表按块取最优名次合并（F4a：BM25(改写)∪BM25(原始)），
// 重排取前 limit——纯增量：原始列表的块名次只会变好不会变差。
func unionLexicalBest(a, b []storage.DocumentChunkHit, limit int) []storage.DocumentChunkHit {
	type ent struct {
		hit  storage.DocumentChunkHit
		rank int
	}
	best := make(map[string]ent, len(a)+len(b))
	add := func(list []storage.DocumentChunkHit) {
		for i, h := range list {
			k := chunkKeyOf(h)
			if old, ok := best[k]; !ok || i < old.rank {
				best[k] = ent{h, i}
			}
		}
	}
	add(a)
	add(b)
	outs := make([]ent, 0, len(best))
	for _, e := range best {
		outs = append(outs, e)
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].rank < outs[j].rank })
	if limit > 0 && len(outs) > limit {
		outs = outs[:limit]
	}
	out := make([]storage.DocumentChunkHit, 0, len(outs))
	for _, e := range outs {
		out = append(out, e.hit)
	}
	return out
}

// f4aBucket 预注册归因树（同 miss autopsy）：仅对 miss 题归类，hit 返回 HIT。
func f4aBucket(e, l, v, fin int) string {
	switch {
	case fin > 0:
		return "HIT"
	case e == 0:
		return "P1"
	case l == 0 && v == 0:
		return "P2"
	case l == 0:
		return "P3"
	case v == 0:
		return "P4"
	default:
		return "P5"
	}
}

func TestQueryRewriteF4A(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("F4a 判定口径 = 云混合；设置 ABSORB_CLOUD_EMBED=1 后运行")
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
		rwModel = "Qwen/Qwen2.5-7B-Instruct"
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
	t.Logf("镜像：%d 块 / %d 向量（%s）｜改写器：%s｜判定线：R@5≥65%% 且 MRR≥0.44 且对照 4/4",
		len(chunks), len(vrows), emb.Model(), rwModel)

	a := document.SharedAnalyzer()
	type caseOut struct {
		name                  string
		rankPlain, rankF4a    int
		bucketPlain, bucketF4 string
		rwMs                  int64
		rewritten             string
		fallback              bool
	}
	var (
		results   []caseOut
		fallbackN int
		rwMsSum   int64
	)

	for _, c := range synonymCases {
		out := caseOut{name: c.name}

		// 锚点块集合（滑窗切块下 0/1/多皆可能）
		keys := make(map[string]struct{}, 2)
		for _, ch := range chunks {
			if strings.Contains(ch.Content, c.anchor) {
				keys[chunkKeyOf(storage.DocumentChunkHit{DocumentID: mdoc.ID, ChunkIndex: ch.ChunkIndex})] = struct{}{}
			}
		}
		e := len(keys)

		// 共享原始检索：词法 top50 + 向量 top50（一次 EmbedQuery 两腿共用）
		qexpr := a.QueryExpression(c.query)
		lexOrig, err := st.SearchDocumentChunks(ctx, app, user, qexpr, []string{mdoc.ID}, 50)
		if err != nil {
			t.Fatalf("lex %q: %v", c.query, err)
		}
		qv, err := emb.EmbedQuery(ctx, c.query)
		if err != nil {
			t.Fatalf("embed %q: %v", c.query, err)
		}
		vec50 := retrieval.TopKByCosine(vhits, vrows, qv, 50)

		// plain 腿：原始管线（与生产/冻结口径完全一致）
		fusedP := retrieval.FuseRRF(lexOrig, vec50, 50)
		rrP, _ := retrieval.RerankCandidates(ctx, fusedP, c.query, reranker, rrTopN)
		finalP, _ := searchdoc.DeduplicateHits(rrP, 10)
		for i, h := range finalP {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				out.rankPlain = i + 1
				break
			}
		}
		out.bucketPlain = f4aBucket(e, rankAmong(lexOrig, keys), rankAmong(vec50, keys), out.rankPlain)

		// F4a 腿：改写 → BM25(改写) ∪ BM25(原始) → 同款融合/精排（精排 query 用原始问题）
		rwStart := time.Now()
		rw, rerr := f4aRewrite(ctx, embKey, rwModel, c.query)
		out.rwMs = time.Since(rwStart).Milliseconds()
		if rerr != nil {
			out.fallback = true
			out.rewritten = "(降级:" + rerr.Error() + ")"
			fallbackN++
		} else {
			out.rewritten = rw
		}
		rwQuery := c.query
		if rerr == nil {
			rwQuery = rw
		}
		var lexUnion []storage.DocumentChunkHit
		if rerr == nil {
			lexRew, lerr := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(rwQuery), []string{mdoc.ID}, 50)
			if lerr != nil {
				t.Fatalf("lex rewritten %q: %v", rwQuery, lerr)
			}
			lexUnion = unionLexicalBest(lexOrig, lexRew, 50)
		} else {
			lexUnion = lexOrig // 降级：纯原始词法
		}
		fusedR := retrieval.FuseRRF(lexUnion, vec50, 50)
		rrR, _ := retrieval.RerankCandidates(ctx, fusedR, c.query, reranker, rrTopN)
		finalR, _ := searchdoc.DeduplicateHits(rrR, 10)
		for i, h := range finalR {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				out.rankF4a = i + 1
				break
			}
		}
		out.bucketF4 = f4aBucket(e, rankAmong(lexUnion, keys), rankAmong(vec50, keys), out.rankF4a)
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
	t.Logf("%-24s %-9s %-9s %-6s %s", "题目", "plain", "F4a", "rwms", "改写输出")
	var (
		p5, p10, mrrP  float64
		f5, f10, mrrF  float64
		synN           float64
		ctlMissP, ctlF int
		migr           = map[string]map[string]int{} // plain 桶 → F4a 结局
	)
	for _, r := range results {
		isSyn := !strings.HasPrefix(r.name, "对照")
		t.Logf("%-24s %-9s %-9s %-6d %s",
			r.name, rankLabel(r.rankPlain)+"("+r.bucketPlain+")",
			rankLabel(r.rankF4a)+"("+r.bucketF4+")", r.rwMs, trunc(r.rewritten, 28))
		if !isSyn {
			if r.rankPlain == 0 {
				ctlMissP++
			}
			if r.rankF4a == 0 {
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
		if r.rankF4a > 0 {
			mrrF += 1 / float64(r.rankF4a)
			if r.rankF4a <= 5 {
				f5++
			}
			f10++
		}
		from := r.bucketPlain
		if from == "HIT" {
			from = "HIT"
		}
		to := r.bucketF4
		if migr[from] == nil {
			migr[from] = map[string]int{}
		}
		migr[from][to]++
	}

	n := synN
	t.Logf("")
	t.Logf("==== F4a 配对评测（同义组 %d 题，云混合单轮）====", int(n))
	t.Logf("plain：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", p5/n*100, p10/n*100, mrrP/n)
	t.Logf("F4a  ：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", f5/n*100, f10/n*100, mrrF/n)
	t.Logf("Δ    ：Recall@5=%+.0fpp  Recall@10=%+.0fpp  MRR=%+.2f", (f5-p5)/n*100, (f10-p10)/n*100, (mrrF-mrrP)/n)
	t.Logf("对照组：plain miss=%d  F4a miss=%d（判定线要求 4/4 保持命中）", ctlMissP, ctlF)
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
	t.Logf("切换线（预注册）：两轮均值 R@5 ≥ 65%% 且 MRR ≥ 0.44 且对照 4/4 → 转正")
}
