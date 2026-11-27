package document_test

// 中文同义表达检索评测（M3 启动决策数据，doc1 §十一 / doc2 §八）：
// 测 M1 词法检索（GSE+FTS5+BM25）在「问句用词 ≠ 原文用词」场景下的召回缺口，
// 为「是否值得接入 Embedding」提供基线数据。
//
// 题目分两组：
//   - 同义组：问句是口语化/换词表达，证据在原文中用完全不同的词——词法检索
//     的预期缺口（观察主体，不设硬门槛，输出数据供决策）。
//   - 对照组：问句与原文同词——词法必须命中，否则说明评测集/索引本身有问题。
//
// 锚点（anchor）为原文中独一无二的短句，且保证单行内（书的提取文本存在
// PDF 乱序与断行，锚点跨行会导致 Contains 误判）。判定：检索结果的 chunk
// 原文包含锚点即算命中该证据。新题锚点须经 SQL 验证原文唯一。
// 当前 48 题 = 44 同义 + 4 对照（第一批 8+2，第二批 2026-11-15 扩 8+1，
// 第三批 2026-11-15 扩到 40+：28+1）。
//
// ── 冻结规则（2026-11-17，v1.0）──────────────────────────────
// 本集 48 题与判定口径自 v1.0 起冻结：增删题、改锚点、改判定口径任一变更，
// 须 EvalSetVersion +1、重跑全量基线、并在本注释记录变更原因与新旧成绩对照。
// 禁止就地修改——评测集是尺子，不是调参对象（doc2 §八「不让评测数据跟着
// 模型调参变化」）。
//
// ── v1.0 基线（44 同义题；评测管线与生产同链路）──────────────
//   词法基线（GSE+FTS5）：                         23% / 30% / MRR 0.20
//   本地 bge-small-zh + 本地精排（CPU 11.7s/题）：  36% / 39% / MRR 0.37
//   云 Qwen3-Embedding-4B + 云 Qwen3-Reranker-4B
//   （topN=16，查询 ~500ms）—— 生产终版：          45% / 45% / MRR 0.34
//
// 噪声带：FTS5 bm25 同分 tie-break 使排名 8~10 的边缘题在多次运行间漂移
// （实测同代码同库两轮 Recall@10 差 1 题），单次运行的指标波动带约 ±2pp。
// 解读规则：两配置差异 <4pp 视为噪声内持平，≥4pp 方可下方向性结论；
// 严格对比建议同配置跑 2~3 次取命中题并集/中位数。
//
// 外部测试包（document_test）：hybrid 评测需 import tool/search_document，
// 该包反向 import internal/document，内部测试包会成环。
//
// 运行（需验收副本，缺失自动 Skip）：
//
//	Copy-Item "$env:APPDATA\absorb-agent\data\sessions.db" "$env:TEMP\absorb-m1-check.db"
//	go test ./internal/document/ -run TestSynonymRetrievalBaseline -v
//	# 云模式（生产终版口径，ABSORB_EMB_MODEL/ABSORB_RR_MODEL 可换模型）：
//	$env:ABSORB_CLOUD_EMBED="1"; $env:ABSORB_EMB_API_KEY="<siliconflow key>"

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/document"
	"absorb-agent/internal/retrieval"
	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
	searchdoc "absorb-agent/tool/search_document"
)

const synonymDBCopy = "absorb-m1-check.db"

// EvalSetVersion 同义评测集版本：v1.0（2026-11-17）冻结 48 题 + 判定口径。
// 变更规则见文件头「冻结规则」——改题必须升版本并重跑基线。
const EvalSetVersion = 1

type synonymCase struct {
	name   string // 同义类型
	query  string // 用户问法（同义表达）
	anchor string // 原文证据锚点（唯一短句，单行内）
}

// 云 embedding（ABSORB_CLOUD_EMBED=1，生产同款组件）；未启用时跑词法基线
// （M3 前基线 38%/50% 的口径）。

var synonymCases = []synonymCase{
	// ---- 同义组（观察主体）----
	{name: "同义-审查重心转移", query: "为什么审查的精力应该放在业务规则而不是代码风格上？", anchor: "转向验证业务逻"},
	{name: "同义-AI队友执行特征", query: "AI 队友干活有什么特点？", anchor: "以机器速度和规模执行任务"},
	{name: "同义-看不到全局", query: "为什么团队成员总是看不见整体？", anchor: "隧道视野悖论"},
	{name: "同义-总承包商类比", query: "建筑行业的总包模式能给软件团队什么启发？", anchor: "作为明确的集成角色应运而生"},
	{name: "同义-规则管不住AI", query: "为什么光靠定规矩没办法让 AI 听话？", anchor: "治理救不了错误的基础"},
	{name: "同义-需求收敛结构", query: "需求讨论用什么结构才不会越聊越散？", anchor: "卡片、对话、确认"},
	{name: "同义-工作台分离", query: "人和 AI 的开发环境为什么要分开？", anchor: "双工作台模型最清晰地指明了方向"},
	{name: "同义-统一认知", query: "怎样才能让整个团队对系统的理解保持一致？", anchor: "共享心智模型和概念完整性"},
	// 第二批（2026-11-15 扩充，覆盖部署/评审/判断力/工件/集成/流水线/语言/重复代码八个领域；
	// 锚点均经 SQL 验证原文唯一且单行内）
	{name: "同义-部署止损", query: "部署之后出了问题怎么快速止损？", anchor: "金丝雀计划和回滚触发条件"},
	{name: "同义-跨部门评审迟", query: "为什么跨部门评审总是姗姗来迟？", anchor: "凑齐合适的人在政治和后勤上代价太高"},
	{name: "同义-判断力培养", query: "怎么让团队在复杂决策上更靠谱？", anchor: "结构化的换位思考来训练"},
	{name: "同义-工件驱动核心", query: "这套方法论围绕什么运转？", anchor: "智能体软件工程是工件驱动的"},
	{name: "同义-合并时机", query: "把代码合进主干，什么时候合重要吗？", anchor: "关于工作流的战略决策"},
	{name: "同义-流水线自主", query: "多智能体流水线怎么设计才能少让人盯着？", anchor: "清晰进入和退出标准"},
	{name: "同义-语言双刃剑", query: "为什么编程语言越灵活反而越危险？", anchor: "能编译但语义上极难审计"},
	{name: "同义-重复代码翻案", query: "为什么 AI 时代不必再痛恨重复代码？", anchor: "派去满代码库传播变更"},
	// 第三批（2026-11-15 扩题到 40+：全书 20 个区段采样，锚点均经 SQL 验证唯一且单行内；
	// 「审计追踪默认自动生成并冻结」「领骑在前，无人轮换」多现但属同一概念多处表述，保留）
	{name: "同义-概率失败组件", query: "怎么跟会犯错的 AI 一起干出靠谱的软件？", anchor: "以一定概率失败的组件之上"},
	{name: "同义-工程非完美件", query: "传统工程是怎么对付不完美材料的？", anchor: "组装完美零件的艺术"},
	{name: "同义-只换工具陷阱", query: "只给团队上 AI 工具不改其他，会出什么事？", anchor: "自动化失败和信任崩塌的最快路径"},
	{name: "同义-队友三责任", query: "把 AI 当队友后，人要担起什么？", anchor: "人类便立刻肩负起三项责任"},
	{name: "同义-技术债时机", query: "什么时候该下手处理技术债？", anchor: "嗅到未来成本的味道"},
	{name: "同义-图替代口头对齐", query: "团队口头说好但后面对不上号怎么办？", anchor: "转瞬即逝的口头对齐、心照不宣的结构"},
	{name: "同义-修错修一类", query: "出了 bug 只修这一个就够了吗？", anchor: "杜绝其整个类别"},
	{name: "同义-AI毛病共通", query: "AI 队友的毛病是它独有的吗？", anchor: "些弱点大多人类也有"},
	{name: "同义-人不可编程", query: "人和 AI 在可靠性治理上最根本的差别？", anchor: "无法对人类进行“编程”"},
	{name: "同义-协作数据包", query: "团队协作的基本单元应该是什么？", anchor: "具有持久链接的数据包"},
	{name: "同义-多方案决策", query: "为什么团队要做多个方案再挑？", anchor: "计划与证据包已成为常规决策手段"},
	{name: "同义-教AI匹配意图", query: "怎么教 AI 队友做事才靠谱？", anchor: "机制必须匹配意图"},
	{name: "同义-强保证构造", query: "怎么保证 AI 一定按规定办事？", anchor: "调用和遵守都由构造保证"},
	{name: "同义-防边界瞎猜", query: "怎么避免 AI 在职责边界外自作主张？", anchor: "防止智能体在边界上瞎猜"},
	{name: "同义-迭代停止依据", query: "迭代停下来的正确依据是什么？", anchor: "迭代是因“人累了”而停止"},
	{name: "同义-过早拍板风险", query: "还没验证就急着定方案有什么风险？", anchor: "过早收敛、虚假的确定性"},
	{name: "同义-并行前提", query: "什么时候不该搞并行开发？", anchor: "每个流的验收目标都说不清"},
	{name: "同义-拆任务接缝", query: "拆任务应该按什么拆？", anchor: "按自然的接缝来分解"},
	{name: "同义-评审会存在", query: "架构评审委员会这类机制为什么存在？", anchor: "设立架构评审委员会"},
	{name: "同义-教训入系统", query: "为什么 AI 团队吃过亏还会重蹈覆辙？", anchor: "必须由系统本身承载这些教训"},
	{name: "同义-防范围蔓延", query: "怎么防止 AI 干着干着范围越干越大？", anchor: "以改进之名行范畴扩张之实"},
	{name: "同义-卡片大小", query: "上下文片段切多大合适？", anchor: "加载时不会造成淹没"},
	{name: "同义-知识治理", query: "内部知识要怎么管才能被 AI 用起来？", anchor: "构建块应是版本化、可查询"},
	{name: "同义-权限方便症", query: "为什么 AI 的权限不能只图方便？", anchor: "不受上下文约束"},
	{name: "同义-审计自动化", query: "审计日志靠人自觉记录行不行？", anchor: "审计追踪默认自动生成并冻结"},
	{name: "同义-车库法拉利", query: "只读书不动手会怎样？", anchor: "停在车库里的法拉利"},
	{name: "同义-引入非问题", query: "现在还在纠结要不要用 AI，姿势对吗？", anchor: "不在于你是否要引入 AI 队友"},
	{name: "同义-领骑无人换", query: "AI 时代的先行者要付出什么代价？", anchor: "领骑在前，无人轮换"},
	// ---- 对照组（评测集有效性校验）----
	{name: "对照-二元性原词", query: "人本软件工程和智本软件工程的二元性是怎么回事？", anchor: "这种二元性无关哲学"},
	{name: "对照-内存安全原词", query: "内存安全问题为什么被看得这么重？", anchor: "内存安全是最具体、最清晰的例子"},
	{name: "对照-合规工程原词", query: "合规工程为什么是必需的？", anchor: "合规工程之所以必要"},
	{name: "对照-信任边界原词", query: "AI 队友必须在谁的信任边界内活动？", anchor: "在指导者的信任边界内活动"},
}

func TestSynonymRetrievalBaseline(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), synonymDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过；见文件头注释", dbPath)
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
	document.NewService(st, nil, app, user).BuildIndex(ctx, doc.ID, dc) // 幂等
	// 索引就绪校验
	if state, _ := st.LoadDocumentIndexState(ctx, app, user, doc.ID); state == nil || state.IndexStatus != storage.IndexStatusReady {
		t.Fatalf("index not ready: %+v", state)
	}

	// 语义路开关：ABSORB_CLOUD_EMBED=1（云 API，硅基流动）。评测走与
	// search_document 完全一致的生产管线（词法+余弦→RRF→[精排]→多样性）。
	cloud := os.Getenv("ABSORB_CLOUD_EMBED") == "1"
	hybrid := cloud
	var emb retrieval.Embedder
	var reranker retrieval.Reranker // 配置时启用精排（云模型）
	rrTopN := 16                    // 精排候选数（ABSORB_RERANK_TOPN 可调）
	var embModel string
	if cloud {
		embKey := os.Getenv("ABSORB_EMB_API_KEY")
		if embKey == "" {
			t.Fatal("cloud 模式需要 ABSORB_EMB_API_KEY")
		}
		embModel = os.Getenv("ABSORB_EMB_MODEL")
		if embModel == "" {
			embModel = "Qwen/Qwen3-Embedding-4B"
		}
		emb = retrieval.NewOpenAICompatEmbedder("", embKey, embModel, 32, nil)
		retrieval.SetSharedEmbedder(emb)
		// 云精排：同 key 同域名 /v1/rerank（ABSORB_RR_MODEL 可换模型）
		rrModel := os.Getenv("ABSORB_RR_MODEL")
		if rrModel == "" {
			rrModel = "Qwen/Qwen3-Reranker-4B"
		}
		reranker = retrieval.NewLlamaRerankClient("https://api.siliconflow.cn/v1", rrModel, embKey, nil)
		t.Logf("mode = HYBRID（云 %s + 云精排 %s，topN=%d）", embModel, rrModel, rrTopN)
	}
	if hybrid {
		// 分身 URL：强制全量重建（原 URL 词法幂等跳过，向量不会补齐）。
		// 幂等按词法/向量双域判断（0010 起）：分身上次中断留下的「词法 ready、
		// 向量缺失」会被 BuildIndex 独立检测并只补向量，无需手动标 failed。
		mirrorURL := bookURL + "?hybrid-eval=1"
		mdc := *dc
		mdc.URL = mirrorURL
		mdoc, err := st.UpsertDocument(ctx, app, user, mirrorURL, "AgenticSE Book CN (hybrid)")
		if err != nil {
			t.Fatalf("upsert mirror: %v", err)
		}
		if err := st.SaveDocumentContent(ctx, app, user, &mdc); err != nil {
			t.Fatalf("save mirror content: %v", err)
		}
		document.NewService(st, nil, app, user).WithEmbedder(emb).BuildIndex(ctx, mdoc.ID, &mdc)
		if _, vrows, verr := st.LoadVectorHits(ctx, app, user, emb.Model(), []string{mdoc.ID}); verr != nil || len(vrows) == 0 {
			t.Fatalf("分身文档向量未入库 (err=%v, rows=%d)——HYBRID 评测无效", verr, len(vrows))
		} else {
			t.Logf("分身向量入库 rows=%d (model=%s)", len(vrows), emb.Model())
		}
	} else {
		t.Log("mode = 词法基线（未启用 ABSORB_CLOUD_EMBED）")
	}

	// 锚点必须在原文中真实存在（评测集自检，防止锚点笔误造成假 miss）
	for _, c := range synonymCases {
		if !strings.Contains(dc.ContentText, c.anchor) {
			t.Fatalf("评测集自检失败：锚点 %q 不在原文中（题目 %s）", c.anchor, c.name)
		}
	}
	t.Logf("evalset v%d | %d 题 = %d 同义 + %d 对照（v1.0 冻结 2026-11-17）",
		EvalSetVersion, len(synonymCases), len(synonymCases)-4, 4)

	a := document.SharedAnalyzer()
	type result struct {
		name    string
		rank    int // 1-based top10 排名；0=前 10 未命中
		rankRaw int // 原始召回（50）内排名；0=未召回
		hits    int
		durMs   int64
	}
	var (
		synResults, ctlResults []result
		recallAt5, recallAt10  float64
		mrrSum                 float64
		rrFallbackCnt          int // 精排失败降级融合序的条数（>0 时 Reranker 口径部分失真）
	)
	t.Logf("%-28s %-6s %-6s %s", "题目", "命中", "MRR", "耗时")
	for _, c := range synonymCases {
		start := time.Now()
		lexical, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.query), nil, 50)
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		hits := lexical
		if hybrid && emb != nil {
			// 生产同款双路管线：余弦召回 → RRF → 多样性（口径与 search_document 一致）
			if qv, qerr := emb.EmbedQuery(ctx, c.query); qerr == nil {
				if vhits, vrows, verr := st.LoadVectorHits(ctx, app, user, emb.Model(), nil); verr == nil {
					vector := retrieval.TopKByCosine(vhits, vrows, qv, 50)
					fused := retrieval.FuseRRF(lexical, vector, 50)
					if reranker != nil {
						var applied bool
						fused, applied = retrieval.RerankCandidates(ctx, fused, c.query, reranker, rrTopN)
						if !applied {
							rrFallbackCnt++
							t.Logf("rerank 失败降级融合序（%s）——本条 Reranker 口径无效", c.name)
						}
					}
					hits, _ = searchdoc.DeduplicateHits(fused, 10)
				}
			}
		} else {
			// 词法模式同口径：只去重 + 总量 10（单文档范围不做 per-doc 配额，
			// 与生产 scoped 路径一致；否则 Recall@5/10 被压到 3 条候选失真）
			hits, _ = searchdoc.DeduplicateHits(lexical, 10)
		}
		dur := time.Since(start).Milliseconds()
		rank, rankRaw := 0, 0
		for i, h := range hits {
			if rank == 0 && i < 10 && strings.Contains(h.Content, c.anchor) {
				rank = i + 1
			}
			if rankRaw == 0 && strings.Contains(h.Content, c.anchor) {
				rankRaw = i + 1
			}
			if rank > 0 {
				break
			}
		}
		r := result{name: c.name, rank: rank, rankRaw: rankRaw, hits: len(hits), durMs: dur}
		isSyn := !strings.HasPrefix(c.name, "对照")
		if isSyn {
			synResults = append(synResults, r)
		} else {
			ctlResults = append(ctlResults, r)
		}
		mark := "✗"
		if rank > 0 {
			mark = "✓"
			// MRR 与 Recall 同口径：只统计同义组（对照组是正向可达性校验，
			// 混入分子会系统性抬高成绩）
			if isSyn {
				mrrSum += 1.0 / float64(rank)
				if rank <= 5 {
					recallAt5++
				}
				recallAt10++
			}
		}
		t.Logf("%-30s %-6s %-6s %dms (hits=%d, raw_rank=%d)", c.name, mark, rankLabel(rank), dur, len(hits), rankRaw)
	}

	// 对照组校验「评测集可达性」：锚点所在 chunk 必须能被词法检索召回。用
	// 锚点自身分词查询且 limit 放宽到 200——问题查询的原始召回 50 装不下目标
	// chunk 只说明同词 chunk 竞争激烈（BM25 排名问题），不代表索引异常。
	for _, c := range synonymCases {
		if !strings.HasPrefix(c.name, "对照") {
			continue
		}
		res, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(c.anchor), nil, 200)
		if err != nil {
			t.Fatalf("reachability search %s: %v", c.name, err)
		}
		found := false
		for _, h := range res {
			if strings.Contains(h.Content, c.anchor) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("对照组 %s 锚点 chunk 无法被词法召回——评测集或索引异常，同义组数据不可信", c.name)
		}
	}

	n := float64(len(synResults))
	mrr := mrrSum / n
	t.Logf("")
	modeName := "词法检索基线"
	if hybrid && emb != nil {
		modeName = "HYBRID 混合检索（词法 + " + emb.Model() + "，RRF）"
		if reranker != nil {
			modeName += " + Reranker"
		}
	}
	t.Logf("==== %s 同义评测（M3 决策数据，语料：%d 字）====", modeName, dc.CharCount)
	if reranker != nil && rrFallbackCnt > 0 {
		t.Logf("警告：%d/%d 条精排失败降级融合序，Reranker 口径部分失真，先排查 8937 sidecar 再解读指标",
			rrFallbackCnt, len(synonymCases))
	}
	t.Logf("同义组（%d 题）：Recall@5=%.0f%%  Recall@10=%.0f%%  MRR@10=%.2f", int(n), recallAt5/n*100, recallAt10/n*100, mrr)
	t.Logf("判读：Recall@10 低（如 <50%%）说明同义缺口显著，接 Embedding 收益大；")
	t.Logf("      若 ≥80%%，词法已够用，M3 语义检索优先级降低。")
}

func rankLabel(rank int) string {
	if rank == 0 {
		return "-"
	}
	return "1/" + itoa(rank)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}
