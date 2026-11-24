package document_test

// Book2 泛化评测（2026-11-18）：人月神话-中文-目录-文字版。
//
// 背景：EvalSet v1.0 的 48 题、Chunker v5 定档 900、F4b 扩池精排转正——
// 全部结论均基于 AgenticSE 单书。本集用一本文体、结构、领域都不同的书
// （散文体软件管理经典，poppler 救回分支）验证生产终态形态的泛化性：
// v5 切块 900 + 云 Qwen3-Embedding-4B + F4b 扩池精排（池 = 融合 top16 ∪ 向量 top40）。
//
// 与 synonym_eval_test 的差异：
//   - 语料自包含：本地 PDF 经本地 HTTP + 生产 PDFExtractor（双跑择优 + TOC 剥离）
//     提取落库（URL 固定，内容 hash 稳定 → BuildIndex 幂等，重复运行不重复嵌入），
//     无需开发库预置该文档
//   - 单文档 scope：评测库副本同时含 AgenticSE 及镜像，全部检索与向量按本文档
//     ID 范围，避免跨书串池
//   - 题集独立冻结：book2EvalSetVersion = 1，15 题 = 12 同义 + 3 对照；
//     锚点单行内且全文唯一（dump 全文 Grep 验证 2026-11-18；「缺陷修复引入
//     新 bug」原文两现，取章内正文带前缀形态保唯一）
//
// ── v1 基线（12 同义题；两轮云混合零漂移）──────────────────────
//   词法基线（GSE+FTS5）：                          42% / 42% / MRR 0.21
//   生产终态（云 Qwen3-Embedding-4B + 云精排，
//   池 = 融合 top16 ∪ 向量 top40）：               75% / 100% / MRR 0.63
//
// 泛化结论（2026-11-18）：AgenticSE 主书 64%/68%/0.34，本书 75%/100%/0.63，
// 全面持平或更优——生产终态形态跨书成立。本书 19.4 万字、271 chunks、
// marks=0（poppler 救回书无章节面包屑）未拖累成绩，与 F3 系列「面包屑
// 元数据对检索近中性」结论互证。语料更干净 + 题集规模小是成绩偏高的
// 可能因素，不作单书间绝对比较。
//
// 运行（需 %TEMP%\absorb-m1-check.db 副本，缺失自动 Skip；拷贝命令见
// synonym_eval_test.go 文件头）：
//
//	go test ./internal/document/ -run TestBook2RetrievalGeneralization -count=1 -v
//	# 云模式（生产终态口径）：
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_EMB_API_KEY="<siliconflow key>"
//	$env:ABSORB_EMB_MODEL="Qwen/Qwen3-Embedding-4B"; $env:ABSORB_RR_MODEL="Qwen/Qwen3-Reranker-4B"

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

// book2EvalSetVersion 人月神话题集版本：v1（2026-11-18）冻结 15 题 + 判定口径。
// 冻结规则同 synonym_eval_test.go——改题必须升版本并重跑基线。
const book2EvalSetVersion = 1

const book2URL = "file://docs/pdf/人月神话-中文-目录-文字版.pdf"

type book2Case struct {
	name   string
	query  string
	anchor string
}

var book2Cases = []book2Case{
	// ---- 同义组（问句口语化/换词，证据在原文用不同词）----
	{name: "同义-加人减速", query: "项目延期了，为什么往里加人反而更糟？", anchor: "实际上是延长了，而不是缩短了时间进度"},
	{name: "同义-沟通开销", query: "团队规模变大后，人均产出为什么反而下降？", anchor: "三个人的工作量是两个人的三倍"},
	{name: "同义-测试时间不足", query: "为什么压缩测试进度等于埋雷？", anchor: "不为系统测试安排足够的时间简直就是一场灾难"},
	{name: "同义-乐观误区", query: "为什么程序员总觉得这次不会出问题？", anchor: "我们的乐观主义并不应该是理所应当的"},
	{name: "同义-设计统一性", query: "为什么宁可砍功能也要保持设计风格统一？", anchor: "概念完整性应该是最重要的考虑因素"},
	{name: "同义-架构实现分离", query: "画架构的和写代码的为什么要分开？", anchor: "体系结构同实现必须仔细地区分开来"},
	{name: "同义-精英带队", query: "意见不统一时由谁拍板效率最高？", anchor: "观点的不一致由外科医生单方面来统一"},
	{name: "同义-二次系统", query: "为什么第二次做同类产品更容易翻车？", anchor: "是典型的第二次开发（second-systemeffect）"},
	{name: "同义-银弹不可能", query: "有没有新工具能让软件生产率一夜暴涨十倍？", anchor: "能够独立地许诺在生产率、可靠性或简洁性上取得数量级的提高"},
	{name: "同义-复杂度本质", query: "软件的本质困难到底难在哪？", anchor: "软件的复杂度是必要属性，不是次要因素"},
	{name: "同义-网状交流", query: "为什么正式组织架构图和实际沟通对不上？", anchor: "交流是通过网状结构进行的"},
	{name: "同义-修bug引新bug", query: "修完 bug 就能放心发布吗？", anchor: "程序维护中的一个基本问题是——缺陷修复总会以（20－50）%的机率引入新的 bug"},
	// ---- 对照组（问句与原文同词，词法必须命中）----
	{name: "对照-人月神话原词", query: "为什么说人月是衡量工作规模的危险神话？", anchor: "危险和带有欺骗性的神话"},
	{name: "对照-外科手术原词", query: "外科手术队伍是怎么组建的？", anchor: "以类似外科手术的方式组建"},
	{name: "对照-内在特性原词", query: "软件无法规避的内在特性有哪些？", anchor: "这些无法规避的内在特性"},
}

func TestBook2RetrievalGeneralization(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过；见 synonym_eval_test.go 文件头", dbPath)
	}
	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// 1. 本地 PDF → 本地 HTTP → 生产提取器（双跑择优 + TOC 剥离 + marks）
	bookPath := findRepoPathFrom("docs/pdf/人月神话-中文-目录-文字版.pdf")
	if bookPath == "" {
		t.Skip("样书不存在（docs/pdf/人月神话-中文-目录-文字版.pdf），跳过")
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
	// 内容上限放宽到 200MB（与 absorb-m2verify 同理由）；代理直连
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
		ExtractionVersion: 5, // bookkeeping 字段，BuildIndex 不读；取当前提取版本字面值
		FetchedAt:         time.Now(),
	}
	if marksJSON, merr := json.Marshal(res.Marks); merr == nil && len(res.Marks) > 0 {
		dc.HeadingMarks = string(marksJSON)
	}

	// 2. 落库 + 建索引（云模式带 embedder，向量同趟补齐；hash 稳定 → 幂等）
	const (
		app  = "absorb-reader"
		user = "absorb-user"
	)
	doc, err := st.UpsertDocument(ctx, app, user, book2URL, "人月神话（文字版）")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.SaveDocumentContent(ctx, app, user, dc); err != nil {
		t.Fatalf("save content: %v", err)
	}

	cloud := os.Getenv("ABSORB_CLOUD_EMBED") == "1"
	var emb retrieval.Embedder
	var reranker retrieval.Reranker
	svc := document.NewService(st, nil, app, user)
	if cloud {
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
		emb = retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
		retrieval.SetSharedEmbedder(emb)
		reranker = retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
		svc = svc.WithEmbedder(emb)
		t.Logf("mode = 生产终态（云 %s + 云精排 %s，池 = 融合 top%d ∪ 向量 top40）", embModel, rrModel, rrTopN)
	} else {
		t.Log("mode = 词法基线（未启用 ABSORB_CLOUD_EMBED）")
	}
	svc.BuildIndex(ctx, doc.ID, dc)
	state, _ := st.LoadDocumentIndexState(ctx, app, user, doc.ID)
	if state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Fatalf("index not ready: %+v", state)
	}

	a := document.SharedAnalyzer()
	// 锚点自检：必须在提取正文中真实存在（防锚点笔误假 miss）
	for _, c := range book2Cases {
		if !strings.Contains(dc.ContentText, c.anchor) {
			t.Fatalf("评测集自检失败：锚点 %q 不在正文中（题目 %s）", c.anchor, c.name)
		}
	}
	t.Logf("book2 evalset v%d | %d 题 = %d 同义 + %d 对照 | 正文 %d 字 → %d chunks（marks %d 条）",
		book2EvalSetVersion, len(book2Cases), len(book2Cases)-3, 3, dc.CharCount, state.ChunkCount, len(res.Marks))

	// 云模式向量就绪校验
	var vhits []storage.DocumentChunkHit
	var vrows []storage.ChunkVectorRow
	if cloud {
		vhits, vrows, err = st.LoadVectorHits(ctx, app, user, emb.Model(), []string{doc.ID})
		if err != nil || len(vrows) == 0 {
			t.Fatalf("向量未入库 (model=%s, err=%v, rows=%d)——云评测无效", emb.Model(), err, len(vrows))
		}
		t.Logf("向量入库 rows=%d (model=%s)", len(vrows), emb.Model())
	}

	type result struct {
		name  string
		rank  int // 1-based top10 排名；0=前 10 未命中
		durMs int64
	}
	var (
		results        []result
		recall5, rec10 float64
		mrrSum         float64
		ctlMiss        int
		rrFallbackCnt  int
	)
	for _, c := range book2Cases {
		start := time.Now()
		lex, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.query), []string{doc.ID}, 50)
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		var hits []storage.DocumentChunkHit
		if cloud {
			qv, qerr := emb.EmbedQuery(ctx, c.query)
			if qerr != nil {
				t.Fatalf("embed %q: %v", c.query, qerr)
			}
			vec50 := retrieval.TopKByCosine(vhits, vrows, qv, 50)
			fused := retrieval.FuseRRF(lex, vec50, 50)
			// 生产同款 F4b 扩池精排（池 = 融合 top16 ∪ 向量 seed40，成功分支
			// 与原手写 rerankPoolF4B+RerankCandidates+尾缀完全等价）：
			// 直接调生产函数，避免评测再造一份管线副本。seed40 由函数内部截断。
			pooled, applied := retrieval.RerankPoolCandidates(ctx, fused, vec50, nil, c.query, reranker, retrieval.RerankPoolSpec{TopN: rrTopN, VecSeed: retrieval.RerankSeedTopN})
			if !applied {
				rrFallbackCnt++
				t.Logf("rerank 失败降级融合序（%s）", c.name)
			}
			hits, _ = searchdoc.DeduplicateHits(pooled, 10)
		} else {
			hits, _ = searchdoc.DeduplicateHits(lex, 10)
		}
		dur := time.Since(start).Milliseconds()
		rank := 0
		for i, h := range hits {
			if i < 10 && strings.Contains(h.Content, c.anchor) {
				rank = i + 1
				break
			}
		}
		results = append(results, result{name: c.name, rank: rank, durMs: dur})
	}

	// 汇总
	t.Logf("%-24s %-6s %s", "题目", "命中", "耗时")
	for _, r := range results {
		t.Logf("%-24s %-6s %dms", r.name, rankLabel(r.rank), r.durMs)
	}
	isSyn := func(name string) bool { return !strings.HasPrefix(name, "对照") }
	var synN float64
	for _, r := range results {
		if !isSyn(r.name) {
			if r.rank == 0 {
				ctlMiss++
			}
			continue
		}
		synN++
		if r.rank > 0 {
			mrrSum += 1 / float64(r.rank)
			rec10++
			if r.rank <= 5 {
				recall5++
			}
		}
	}
	t.Logf("")
	t.Logf("==== Book2 人月神话（%d 同义题）====", int(synN))
	if cloud {
		t.Logf("生产终态：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f（rerank 降级 %d 条）",
			recall5/synN*100, rec10/synN*100, mrrSum/synN, rrFallbackCnt)
	} else {
		t.Logf("词法基线：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f",
			recall5/synN*100, rec10/synN*100, mrrSum/synN)
	}
	t.Logf("对照组 miss=%d/3（词法同词题必须命中）", ctlMiss)
}
