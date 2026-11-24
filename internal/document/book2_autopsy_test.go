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

// book2_autopsy_test.go —— Book2（人月神话）miss 逐题归因（2026-11-18）：
//
// 与 TestMissAutopsy（主书）同款 P1~P5 决策树，验证分桶结构跨书成立。
// 差异：①语料自包含（本地 PDF → 本地 HTTP → 生产提取器 → 自建索引/向量，
// 不依赖预置开发库文档）；②HIT 判定走 Book2 生产终态 F4b 扩池口径
// （池 = 融合 top16 ∪ 向量 top40，整池精排 + 融合尾缀后），与
// TestBook2RetrievalGeneralization 一致；③诊断 E/L/V/F 仍按融合榜口径采集，
// 保证与主书归因可比。
//
// HIT 阈值取 top5（对齐 R@5）而非主书的 top10：Book2 在 top10 层面 15/15 全命中
// （2026-11-18 实测，零 top10 miss），若沿用 top10 决策树无 miss 可归因。
// 本测试的使命是暴露 R@5 盲区（实测 3 道：fin=7/7/8）——即「命中但排名靠后」，
// 与 Book2 的 R@5=75% 水位口径一致。
//
//	P1 候选池缺失·跨块切断（E=0，无单块含完整锚点）
//	P2 候选池缺失·双路盲区（L=0 ∧ V=0）
//	P3 词汇不匹配（L=0 ∧ V>0）
//	P4 嵌入稀释（L>0 ∧ V=0）
//	P5 融合截断（L>0 ∧ V>0，三类之外——topN/融合权重问题）
//
// 运行（云混合口径，与 book2 评测同）：
//
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_EMB_API_KEY="<siliconflow key>"
//	go test ./internal/document/ -run TestBook2MissAutopsy -count=1 -v

func TestBook2MissAutopsy(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("归因口径 = 生产云混合形态；设置 ABSORB_CLOUD_EMBED=1 后运行")
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

	// 1. 自包含语料：本地 PDF → 本地 HTTP → 生产提取器（双跑择优+TOC）
	bookPath := findRepoPathFrom("docs/pdf/人月神话-中文-目录-文字版.pdf")
	if bookPath == "" {
		t.Skip("样书不存在，跳过")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/book.pdf", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, bookPath) })
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	ex := extract.NewPDFExtractor(fetch.NewFetchPageService(func() string { return "" },
		func() int64 { return 200 << 20 }), func() string { return "" },
		func() int64 { return 200 << 20 })
	xctx, xcancel := context.WithTimeout(ctx, 5*time.Minute)
	defer xcancel()
	res, err := ex.Extract(xctx, "http://"+ln.Addr().String()+"/book.pdf")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	const (
		app  = "absorb-reader"
		user = "absorb-user"
	)
	doc, err := st.UpsertDocument(ctx, app, user, book2URL, "人月神话（文字版）")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 用 Book2 评测同款 hash 逻辑保证幂等复用其已入库向量（换 key 重建则重新 embed）
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	retrieval.SetSharedEmbedder(emb)
	rr := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
	svc := document.NewService(st, nil, app, user).WithEmbedder(emb)

	// 复用 save + build（与 book2_eval 一致的字节级内容），hash 稳定 → 命中已有向量
	sum := sha256.Sum256([]byte(res.Content))
	dc := &storage.DocumentContent{
		URL: book2URL, ContentText: res.Content,
		CharCount: len([]rune(res.Content)), ContentType: res.ContentType,
		ByteSize: int64(len(res.Content)), ContentHash: hex.EncodeToString(sum[:]),
		ExtractionStatus: extract.StatusReady, ExtractionVersion: 5,
		FetchedAt: time.Now(),
	}
	if err := st.SaveDocumentContent(ctx, app, user, dc); err != nil {
		t.Fatalf("save content: %v", err)
	}
	svc.BuildIndex(ctx, doc.ID, dc)

	vhits, vrows, err := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{doc.ID})
	if err != nil || len(vrows) == 0 {
		t.Fatalf("向量缺失 (model=%s, err=%v)", emb.Model(), err)
	}
	chunks, err := st.LoadDocumentChunks(ctx, app, user, doc.ID)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("块缺失 (err=%v)", err)
	}
	state, _ := st.LoadDocumentIndexState(ctx, app, user, doc.ID)
	t.Logf("Book2: %d 块 / %d 向量（model=%s, status=%s）｜生产管线：词法50+向量50→FuseRRF(50)→扩池精排(融合16∪向量40)→去重10",
		len(chunks), len(vrows), emb.Model(), state.IndexStatus)

	a := document.SharedAnalyzer()

	type rowOut struct {
		name           string
		verdict        string
		e              int
		l, lfull       int
		v, vfull       int
		cos            float64
		f, rr, fin     int
		applied        bool
		tokHit, tokAll int
		tokMissing     string
	}
	rows := make([]rowOut, 0, len(book2Cases))
	rankOfKeys := func(hits []storage.DocumentChunkHit, keys map[string]struct{}) int {
		for i, h := range hits {
			if _, ok := keys[chunkKeyOf(h)]; ok {
				return i + 1
			}
		}
		return 0
	}

	for _, c := range book2Cases {
		row := rowOut{name: c.name}

		// E：包含完整锚点的块
		keys := make(map[string]struct{}, 2)
		firstIdx := -1
		for i, ch := range chunks {
			if strings.Contains(ch.Content, c.anchor) {
				keys[doc.ID+"#"+strconv.Itoa(ch.ChunkIndex)] = struct{}{}
				if firstIdx < 0 {
					firstIdx = i
				}
			}
		}
		row.e = len(keys)

		// 词法路（生产池 50 + 全量）
		qexpr := a.QueryExpression(c.query)
		lex50, err := st.SearchDocumentChunks(ctx, app, user, qexpr, []string{doc.ID}, 50)
		if err != nil {
			t.Fatalf("lex search %q: %v", c.query, err)
		}
		lexFull, err := st.SearchDocumentChunks(ctx, app, user, qexpr, []string{doc.ID}, 400)
		if err != nil {
			t.Fatalf("lex full search %q: %v", c.query, err)
		}
		row.l = rankOfKeys(lex50, keys)
		row.lfull = rankOfKeys(lexFull, keys)

		// 向量路（生产池 50 + 全量）
		qv, err := emb.EmbedQuery(ctx, c.query)
		if err != nil {
			t.Fatalf("embed %q: %v", c.query, err)
		}
		vec50 := retrieval.TopKByCosine(vhits, vrows, qv, 50)
		vecAll := retrieval.TopKByCosine(vhits, vrows, qv, len(vrows))
		row.v = rankOfKeys(vec50, keys)
		row.vfull = rankOfKeys(vecAll, keys)
		if firstIdx >= 0 {
			want := chunks[firstIdx].ChunkIndex
			for i := range vrows {
				if vhits[i].ChunkIndex == want {
					row.cos = cosineOf(qv, vrows[i].Vector)
					break
				}
			}
		}

		// 融合榜（诊断 F）→ 生产 F4b 扩池精排（生产函数，成功分支等价）
		fused := retrieval.FuseRRF(lex50, vec50, 50)
		row.f = rankOfKeys(fused, keys)
		pooled, applied := retrieval.RerankPoolCandidates(ctx, fused, vec50, nil, c.query, rr, retrieval.RerankPoolSpec{TopN: rrTopN, VecSeed: retrieval.RerankSeedTopN})
		row.applied = applied
		row.rr = rankOfKeys(pooled, keys)
		final, _ := searchdoc.DeduplicateHits(pooled, 10)
		for i, h := range final {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				row.fin = i + 1
				break
			}
		}

		// 诊断：锚点块 search_text 对查询 token 的覆盖
		if firstIdx >= 0 {
			stText := chunks[firstIdx].SearchText
			for _, tok := range parseQueryTokens(qexpr) {
				row.tokAll++
				if strings.Contains(stText, tok) {
					row.tokHit++
				} else {
					row.tokMissing += tok + " "
				}
			}
		}

		// 预注册决策树（按序匹配、命中即停）——与主书同款，HIT 阈值改 top5
		switch {
		case row.fin > 0 && row.fin <= 5:
			row.verdict = "HIT"
		case row.fin > 5:
			// top10 命中但 R@5 miss：命中后排名靠后，单列以便与 P1~P5 对照
			row.verdict = "R5靠后"
		case row.e == 0:
			row.verdict = "P1跨块"
		case row.l == 0 && row.v == 0:
			row.verdict = "P2双路盲区"
		case row.l == 0:
			row.verdict = "P3词汇不匹配"
		case row.v == 0:
			row.verdict = "P4嵌入稀释"
		default:
			row.verdict = "P5融合截断"
		}
		rows = append(rows, row)
	}

	// 明细表 + 汇总
	t.Logf("%-24s %-14s %2s %4s %4s %4s %4s %5s %3s %3s %3s %s",
		"题目", "判定", "E", "L", "Lf", "V", "Vf", "cos", "F", "RR", "fin", "块内缺词")
	counts := map[string]int{}
	synMiss, ctlMiss, hitN, r5Late := 0, 0, 0, 0
	p2Near := 0
	rrFallback := 0
	for _, r := range rows {
		rrmark := ""
		if !r.applied {
			rrmark = "*"
			rrFallback++
		}
		t.Logf("%-24s %-13s%s %2d %4d %4d %4d %4d %5.3f %3d %3d %3d [%d/%d %s]",
			r.name, r.verdict, rrmark, r.e, r.l, r.lfull, r.v, r.vfull, r.cos,
			r.f, r.rr, r.fin, r.tokHit, r.tokAll, strings.TrimSpace(r.tokMissing))
		counts[r.verdict]++
		isSyn := !strings.HasPrefix(r.name, "对照")
		if r.verdict == "HIT" {
			hitN++
			continue
		}
		if r.verdict == "R5靠后" {
			r5Late++
		}
		if isSyn {
			synMiss++
		} else {
			ctlMiss++
		}
		if r.verdict == "P2双路盲区" && r.vfull > 0 && r.vfull <= 80 {
			p2Near++
		}
	}

	synN := len(book2Cases) - 3
	t.Logf("")
	t.Logf("==== Book2 Miss Autopsy（同义组 %d 题，云混合单轮，生产 F4b 口径，HIT=top5）====", synN)
	t.Logf("top5 命中 %d ｜ R@5 miss %d（其中 top10 内靠后 %d + top10 外 miss %d）",
		hitN, synMiss+ctlMiss, r5Late, synMiss+ctlMiss-r5Late)
	t.Logf("同义组归因（P1~P5，top10 外 miss 才分桶）：P1跨块=%d  P2双路盲区=%d（Vfull≤80 边缘 %d）  P3词汇不匹配=%d  P4嵌入稀释=%d  P5融合截断=%d",
		counts["P1跨块"], counts["P2双路盲区"], p2Near, counts["P3词汇不匹配"], counts["P4嵌入稀释"], counts["P5融合截断"])
	if r5Late > 0 {
		t.Logf("R@5 靠后题全程 E/L/V 均可达（top10 内命中），瓶颈在精排名次而非候选池——与 P1~P5 的「找不到」性质不同")
	}
	if ctlMiss > 0 {
		t.Logf("对照组 R@5 miss %d——评测集可达性异常，单独排查", ctlMiss)
	}
	if rrFallback > 0 {
		t.Logf("警告：%d 条精排降级（RR 列带 *），相关题归因按 P5 谨慎解读", rrFallback)
	}
	t.Logf("对比基准（主书同义组 top10 口径）：P2=7 P3=3 P4=3 P5=1 边缘竞争=1 → 主导桶 P2")
}
