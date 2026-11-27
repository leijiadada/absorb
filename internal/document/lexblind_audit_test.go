package document_test

// lexblind_audit_test.go —— 词法盲区桶审计（2026-11-20，#5 精排池词法种子的先验验证）：
//
// 假设（机制侧推演，未经验证）：RRF(k=60) 结构性偏好双路命中——任一双路命中
// （1/61+1/110≈0.0255）得分必然高于任何单路第 1 名（1/61≈0.0164），融合榜
// top16 会被双路+向量强命中填满。F4b 池 = 融合 top16 ∪ 向量种子 top40，词法路
// 没有种子：「BM25 强、embedding 弱」的条目（罕见专名/代号/中英混排术语的
// 镜像失败模式）精排永远看不到。且池 ≥16 ≥ final top_k(≤10)，故
// gold ∉ pool ⇒ 最终确定性 miss——桶判定不依赖精排质量。
//
// 桶定义（预注册，先写后跑）：
//   词法盲区桶（C）= miss 题满足 ①gold 在词法 top50（词法看得到）
//                                ②融合排名 >16（被双路挤出）
//                                ③gold 不在向量 top40（向量种子救不了）
//   对照桶：D=双路盲（词法+向量 top50 均无，词汇+语义双重鸿沟）；
//           V=词法盲+向量 41~50（向量种子扩容方向，非词法种子方向）；
//           A=池内精排未救（F4b 口径残余）。
//   判据：44 同义题中 C ≥4 → 立项 F11（池 += 词法 topN，N 按审计名次分布
//   定档 25/50，见 design/5.md F11 预注册）；C ≤1 → 关闭假设。
//   对照组（4 题）出现 miss → 评测集/管线异常，先排查再解读。
//
// 桶判定（池成员资格）只依赖词法/向量/融合名次，与精排模型无关——换精排模型
// 复跑 C/D 桶应完全一致（2026-11-20 实测 4B/8B 双跑一致）；finalRank/R@10 与
// A 桶归因随精排模型变化，按生产现款（F9 8B）口径解读。
//
// 审计为只读观测：完整复刻生产管线（词法+余弦 top50 → RRF → F4b 精排池 →
// 去重 top10）仅为精确划分 miss 归因，不改任何行为与参数。
//
// 运行（云口径，key 取 ABSORB_EMB_API_KEY，缺省回退应用配置 config.yaml）：
//
//	$env:ABSORB_CLOUD_EMBED="1"
//	go test ./internal/document/ -run TestLexBlindSpotAudit -count=1 -v

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/document"
	"absorb-agent/internal/retrieval"
	"absorb-agent/internal/store/storage"
	searchdoc "absorb-agent/tool/search_document"
)

// loadEmbKeyFromConfig 从应用配置回退读取 embedding.api_key（生产同源）。
// 仅在环境变量未设置时使用；key 本身不打印、不落日志。
func loadEmbKeyFromConfig() string {
	p := filepath.Join(os.Getenv("APPDATA"), "absorb-agent", "config", "config.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	inEmb := false
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "embedding:") {
			inEmb = true
			continue
		}
		if !inEmb {
			continue
		}
		// 离开 embedding 块：回到顶层键（无缩进的非注释非空行）
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(t, "#") {
			inEmb = false
			continue
		}
		if strings.HasPrefix(t, "api_key:") {
			v := strings.TrimSpace(strings.TrimPrefix(t, "api_key:"))
			if i := strings.Index(v, "#"); i >= 0 {
				v = v[:i]
			}
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if v != "" {
				return v
			}
		}
	}
	return ""
}

// bestRank 命中名次：hits（1-based）中首个原文含锚点的条目名次，0=未含。
// 锚点唯一但相邻块 180 rune 重叠可致多个块含锚点，取最优名次（与基线判定同口径）。
func bestRank(hits []storage.DocumentChunkHit, anchor string) int {
	for i, h := range hits {
		if strings.Contains(h.Content, anchor) {
			return i + 1
		}
	}
	return 0
}

func poolIcon(b bool) string {
	if b {
		return "IN"
	}
	return "-"
}

func TestLexBlindSpotAudit(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过", dbPath)
	}
	if os.Getenv("ABSORB_CLOUD_EMBED") != "1" {
		t.Skip("审计需要真实向量路；设置 ABSORB_CLOUD_EMBED=1 后运行")
	}
	embKey := os.Getenv("ABSORB_EMB_API_KEY")
	keySrc := "env"
	if embKey == "" {
		embKey = loadEmbKeyFromConfig()
		keySrc = "config.yaml"
	}
	if embKey == "" {
		t.Fatal("云模式需要 ABSORB_EMB_API_KEY 或应用配置 embedding.api_key")
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
	dc, err := st.LoadDocumentContent(ctx, app, user, bookURL)
	if err != nil || dc == nil {
		t.Skipf("副本中无目标文档正文 (err=%v)", err)
	}
	doc, err := st.UpsertDocument(ctx, app, user, bookURL, "AgenticSE Book CN")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	emb := retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
	retrieval.SetSharedEmbedder(emb)
	rr := retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
	t.Logf("mode = 审计（云 %s + 云精排 %s；key 来源=%s）", embModel, rrModel, keySrc)

	// 向量域就绪性：ready 则零额外 embedding 成本复用；缺失/陈旧仅补向量（词法不动）
	document.NewService(st, nil, app, user).WithEmbedder(emb).BuildIndex(ctx, doc.ID, dc)
	if state, _ := st.LoadDocumentIndexState(ctx, app, user, doc.ID); state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Fatalf("index not ready: %+v", state)
	}
	if _, vrows, verr := st.LoadVectorHits(ctx, app, user, emb.Model(), nil); verr != nil || len(vrows) == 0 {
		t.Fatalf("向量未入库 (err=%v, rows=%d)——审计无效", verr, len(vrows))
	} else {
		t.Logf("向量复用 rows=%d (model=%s)", len(vrows), emb.Model())
	}

	// 锚点自检（防笔误假 miss）
	for _, c := range synonymCases {
		if !strings.Contains(dc.ContentText, c.anchor) {
			t.Fatalf("锚点 %q 不在原文中（题目 %s）", c.anchor, c.name)
		}
	}

	a := document.SharedAnalyzer()
	var (
		synHits, synMissD, synMissC int
		synMissCLexTop10            int // C 中词法前 10（词法种子 top10 可直接覆盖）
		synMissCVecBeyond40         int // C 中向量 41~50 可见（换向量种子扩容方向可覆盖）
		synMissVecSeedOut           int // 词法盲 + 向量 41~50（向量种子扩容方向）
		synMissPool                 int // 池内但精排未救（F4b 口径残余）
		ctlHits, ctlMiss            int
		rrFallback                  int
	)
	t.Logf("%-30s %4s %4s %5s %4s %4s %s", "题目", "词法", "向量", "融合", "池", "终榜", "耗时")
	for _, c := range synonymCases {
		start := time.Now()
		lexical, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.query), nil, 50)
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		var vector []storage.DocumentChunkHit
		if qv, qerr := emb.EmbedQuery(ctx, c.query); qerr == nil {
			if vhits, vrows, verr := st.LoadVectorHits(ctx, app, user, emb.Model(), nil); verr == nil {
				vector = retrieval.TopKByCosine(vhits, vrows, qv, 50)
			} else {
				t.Logf("向量加载失败（%s）: %v", c.name, verr)
			}
		} else {
			t.Logf("查询向量化失败（%s）: %v", c.name, qerr)
		}
		fused := retrieval.FuseRRF(lexical, vector, 50)
		// 生产同款精排池（F4b 口径）：池 = 融合 top16 ∪ 向量种子 top40
		pooled, applied := retrieval.RerankPoolCandidates(ctx, fused, vector, nil, c.query, rr, retrieval.RerankPoolSpec{TopN: 16, VecSeed: 40})
		if !applied {
			rrFallback++
		}
		final, _ := searchdoc.DeduplicateHits(pooled, 10)

		lexRank := bestRank(lexical, c.anchor)
		vecRank := bestRank(vector, c.anchor)
		fusedRank := bestRank(fused, c.anchor)
		finalRank := bestRank(final, c.anchor)
		inPool := (fusedRank >= 1 && fusedRank <= 16) || (vecRank >= 1 && vecRank <= 40)

		t.Logf("%-30s %4d %4d %5d %4s %4d %dms", c.name, lexRank, vecRank,
			fusedRank, poolIcon(inPool), finalRank, time.Since(start).Milliseconds())

		switch {
		case strings.HasPrefix(c.name, "对照"):
			if finalRank > 0 {
				ctlHits++
			} else {
				ctlMiss++
			}
		case finalRank > 0:
			synHits++
		case lexRank == 0 && vecRank == 0:
			synMissD++
		case !inPool && lexRank >= 1:
			// 词法盲区桶（本审计主判据）
			synMissC++
			if lexRank <= 10 {
				synMissCLexTop10++
			}
			if vecRank > 40 {
				synMissCVecBeyond40++
			}
		case !inPool:
			synMissVecSeedOut++ // 词法盲 + 向量 41~50：种子扩容方向在向量侧
		default:
			synMissPool++ // inPool：精排未救
		}
	}

	synTotal := len(synonymCases) - 4
	t.Log("────────────────────────── 审计汇总 ──────────────────────────")
	t.Logf("同义 %d 题：最终命中 %d（R@10=%.1f%%）", synTotal, synHits, float64(synHits)/float64(synTotal)*100)
	t.Logf("对照组：%d/4 命中（miss=%d——对照组 miss 属异常，先排查）", ctlHits, ctlMiss)
	t.Logf("miss 归因桶（同义题）：")
	t.Logf("  D 双路盲（词法+向量 top50 均无）：            %d", synMissD)
	t.Logf("  C 词法盲区桶（词法可见+融合>16+向量种子外）：%d  ← 主判据", synMissC)
	t.Logf("    C1 词法前 10（词法种子 top10 可覆盖）：    %d", synMissCLexTop10)
	t.Logf("    C2 向量 41~50（换向量种子扩容可覆盖）：    %d", synMissCVecBeyond40)
	t.Logf("  V 词法盲+向量 41~50（向量种子扩容方向）：     %d", synMissVecSeedOut)
	t.Logf("  A 池内精排未救（F4b 口径残余）：              %d", synMissPool)
	t.Logf("精排降级条数：%d（>0 时归因部分失真）", rrFallback)
	t.Log("────────────────────────── 判据（预注册）────────────────────")
	switch {
	case synMissC >= 4:
		t.Logf("判定：词法盲区桶 C=%d ≥ 4 → 立项 F11（池 += 词法 topN，N 按审计名次分布定档 25/50，见 design/5.md）", synMissC)
	case synMissC <= 1:
		t.Logf("判定：词法盲区桶 C=%d ≤ 1 → 关闭假设（F4b 不对称属数据正确）", synMissC)
	default:
		t.Logf("判定：词法盲区桶 C=%d ∈ (1,4) 灰区 → 补充证据后再决", synMissC)
	}
}
