package chat

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	expand "absorb-agent/tool/expand_context"
	retriever "absorb-agent/tool/pageretriever"
	recall "absorb-agent/tool/recall_history"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
)

// ImageInput 侧栏发来的单张图片。DataURL 为截图类 base64；URL 为网页图公网链接。
// 二者互斥，由 imageSource 解析；校验见 validateImage。
type ImageInput struct {
	DataURL string `json:"dataUrl"`
	URL     string `json:"url"`
	Mime    string `json:"mime"`
}

// ContextInput 划词上下文（扩展端采集）。
type ContextInput struct {
	Text          string `json:"text"`
	Paragraph     string `json:"paragraph"`
	PageContext   string `json:"pageContext"`
	URL           string `json:"url"`
	PageMd        string `json:"pageMd"` // 整页结构化文本（扩展端已转换，直读入缓存，上限 100K 字符）
	ContentStatus string `json:"contentStatus"` // 页面正文完整度：full/partial/selected_only/unavailable（空=不上报，提示词不渲染）
}

// PageInput 当前浏览页：无划词提问时供 agent fetch_page 获取正文。
// DocumentID 为 /api/page/prepare 的产物（document_contents 锚点 = URL），
// 非空时优先注入已提取的文档正文（PDF 解读 PR2）。
type PageInput struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	DocumentID string `json:"documentId"`
}

// InvocationInput 一次问答的请求输入：/api/ask 请求体与评估用例共用同一形状，
// 保证评估路径与生产一致（评估侧由 SessionInput.State 映射而来）。
type InvocationInput struct {
	Question  string        `json:"question"`
	Images    []ImageInput  `json:"images"`
	Context   *ContextInput `json:"context"`
	Page      *PageInput    `json:"page"`
	SessionID string        `json:"sessionId"`
}

// InvocationDeps 组装所需的运行时依赖：生产 AskHandler 与评估入口注入同一套实例。
type InvocationDeps struct {
	SessionSvc *store.SessionService
	PS         PromptStrategy
	WSFn       func() conf.WorkingSetConfig // 工作集配置读取闭包（nil 视为默认启用）
}

// PreparedInvocation 组装结果：调用 runner.Run 所需的全部入参。
// RequestID 由调用方登记到 RunRegistry（生产 /api/cancel 用；评估侧可忽略）。
type PreparedInvocation struct {
	SessionID string
	RequestID string
	FilterKey string // scope 身份（文章哈希或 kind.ScopeGeneral）
	ScopeURL  string // 可读网址（日志/调试；空串表示无页面上下文）
	Message   model.Message
	RunCtx    context.Context
	RunOpts   []agent.RunOption
}

// PrepareInvocation 把一次问答输入组装成 runner.Run 入参：会话定位 → scope 继承 →
// 参考材料重申 → 页面缓存 → 提示词构造 → 工具状态注入 → 工作集 L1。
// 生产 AskHandler 与评估链路（cmd/absorb-eval 的 runner 包装器）共用，保证评估与生产一致。
// 仅工作集 L1 超预算返回错误（生产侧转 SSE 错误终止本轮）；其余失败路径照生产降级只记日志。
func PrepareInvocation(ctx context.Context, app, user string, in InvocationInput, deps InvocationDeps) (*PreparedInvocation, error) {
	sessionSvc := deps.SessionSvc
	ps := deps.PS

	sessionID := in.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	askCtx := AskContext{Question: in.Question}
	if in.Context != nil {
		askCtx.SelectedText = in.Context.Text
		askCtx.Paragraph = in.Context.Paragraph
		askCtx.PageContext = in.Context.PageContext
		askCtx.URL = in.Context.URL
		askCtx.PageMd = in.Context.PageMd
		askCtx.ContentStatus = in.Context.ContentStatus
	}
	// 页面级上下文：无划词提问时兜底取当前浏览页；划词来源 URL 优先，不覆盖
	if in.Page != nil && strings.TrimSpace(askCtx.URL) == "" {
		askCtx.URL = in.Page.URL
		askCtx.PageTitle = in.Page.Title
	}
	// scope 标记：划词来源 URL 优先；追问无新划词时从会话历史继承最近一次
	// <page_url>；仍无（新会话首轮无页面上下文）落 kind.ScopeGeneral。打标后：
	// 本轮事件落库自动继承 FilterKey、请求构建按 scope 隔离其他文章历史、
	// 摘要按 scope 生成/注入。
	// 须先于追问重申解析，重申查询按当前 scope 隔离。
	scopeURL := strings.TrimSpace(askCtx.URL)
	if scopeURL == "" && sessionID != "" {
		if inherited, err := sessionSvc.RecentPageURL(ctx, app, user, sessionID); err != nil {
			log.Warnf("[Absorb] inherit page url failed: %v", err)
		} else {
			scopeURL = strings.TrimSpace(inherited)
		}
	}
	// scope 身份：URL 归一后取定长哈希作为 filterKey。哈希不含 "/" 且互不为前缀，
	// SDK 的分支过滤/摘要前缀聚合/scope 过滤全部退化为精确匹配，目录页提问不会被
	// 子孙页面的摘要聚合或 cutoff 波及。人类可读网址另存 scope_urls 映射（日志/调试），
	// 不参与身份判定。无页面轮次落 kind.ScopeGeneral（不再留空标，见 kind/scope.go）。
	filterKey := store.ScopeID(scopeURL)
	if filterKey == "" {
		filterKey = kind.ScopeGeneral
	}
	if sessionID != "" {
		if err := sessionSvc.UpsertScopeURL(ctx, app, user, sessionID, filterKey, scopeReadable(scopeURL)); err != nil {
			// 映射只影响日志/调试可读性，失败不阻断问答
			log.Warnf("[Absorb] upsert scope url mapping failed (session=%s): %v", sessionID, err)
		}
	}

	// 追问重申：无新划词时，从当前 scope 的会话历史查最近 <reference_material>
	// 注入当前 user message（scope 内从未划词则不重申，不串其他文章的参考材料）
	if strings.TrimSpace(askCtx.SelectedText) == "" && sessionID != "" {
		if rm, err := sessionSvc.RecentReferenceMaterial(ctx, app, user, sessionID, filterKey); err != nil {
			log.Errorf("[Absorb] query historical reference material failed: %v", err)
		} else if rm != "" {
			askCtx.ReferenceMaterial = rm
			log.Infof("[Absorb] restated reference material for follow-up (%d chars)", len(rm))
		}
	}

	// 页面文本缓存（expand_context 本地检索用，scope 感知：同会话多页面各存一行）：
	// 带新划词 → pageMd（扩展端已转换，免转换直读）写入该 scope 缓存（追问可复用）；
	// 无新划词（追问）/pageMd 为空（转换失败等）→ 按当前 scope 读缓存，查无则空
	// （宁缺勿错，不串其他页面）；都无 → 检索工具返回无缓存失败。
	pageText := ""
	if in.Context != nil && strings.TrimSpace(in.Context.PageMd) != "" {
		// 截断守卫不可省：直读路径没有转换器内置的 pageTextMaxLen 截断，
		// 超长 payload 须经同一规格截断后再入缓存。
		pageText = retriever.TruncatePageText(in.Context.PageMd)
	}
	if pageText != "" {
		if err := sessionSvc.SavePageText(ctx, app, user, sessionID, filterKey, askCtx.PageTitle, pageText); err != nil {
			log.Errorf("[Absorb] save page cache failed: %v", err)
		} else {
			log.Infof("[Absorb] page cache updated (scope=%q %d chars)", filterKey, len(pageText))
		}
	} else if sessionID != "" {
		if t, err := sessionSvc.LoadPageText(ctx, app, user, sessionID, filterKey); err != nil {
			log.Errorf("[Absorb] read page cache failed: %v", err)
		} else {
			pageText = t
		}
	}
	// 文档上下文（PDF 解读 PR2）：页面携带 documentId（/api/page/prepare 产物）
	// 时注入已提取正文，覆盖网页快照路径的 pageText（文档正文完整度高于快照）。
	// 同时写入会话页面缓存，后续追问（无 documentId 轮）经既有 LoadPageText 复用，
	// 不再重复读取 document_contents。注入前统一按 PageTextMaxLen 截断（与
	// pageMd 直读路径同一守卫）。
	if in.Page != nil {
		if docID := strings.TrimSpace(in.Page.DocumentID); docID != "" {
			dc, derr := sessionSvc.Storage().LoadDocumentContent(ctx, app, user, docID)
			switch {
			case derr != nil:
				log.Warnf("[Absorb] load document content failed (doc=%s): %v", docID, derr)
			case dc == nil || !extract.Injectable(dc.ExtractionStatus):
				status := ""
				if dc != nil {
					status = dc.ExtractionStatus
				}
				log.Infof("[Absorb] document not injectable (doc=%s status=%q)", docID, status)
			default:
				pageText = retriever.TruncatePageText(dc.ContentText)
				// 回写 askCtx.PageMd：prompt 侧 <page_content> 只从该字段渲染
				// （pageContentBlock）。不回写则模型看不到正文在场，还会被
				// <page_url> 的"按需 fetch_page"提示误导（正文已在手，fetch 徒劳）。
				// 截断后 ≤ PageTextMaxLen，pageContentBlock 再取头部 6000 字注入，
				// 全量由 expand_context 检索——与 pageMd 直读路径同一套预算。
				askCtx.PageMd = pageText
				// 标记文档场景：划词提问也注入 <page_content>
				askCtx.DocumentID = docID
				log.Infof("[Absorb] document content injected (doc=%s status=%s stored=%d chars injected<=%d)",
					docID, dc.ExtractionStatus, dc.CharCount, retriever.PageTextMaxLen)
				if err := sessionSvc.SavePageText(ctx, app, user, sessionID, filterKey, askCtx.PageTitle, pageText); err != nil {
					log.Errorf("[Absorb] save document page cache failed: %v", err)
				}
			}
		}
	}
	// 参考材料优先取追问重申结果（多轮语义），首轮取划选文本
	refMat := askCtx.ReferenceMaterial
	if strings.TrimSpace(refMat) == "" {
		refMat = askCtx.SelectedText
	}
	recallKey := session.Key{AppName: app, UserID: user, SessionID: sessionID}

	prompt := ps.BuildUserMessage(askCtx)
	log.Debugf("[Absorb] [%s] prompt:\n%s\n---", ps.ID(), prompt)
	msg := model.NewUserMessage(prompt)
	for _, im := range in.Images { // 文本 prompt 不变（前端已带“请解读图片内容”），仅追加图片 part
		msg.AddImageURL(imageSource(im), "auto") // src 为 base64 或 http(s) 链接均兼容
	}
	// 注入页面检索状态（缓存全文+参考材料），expand_context 工具从 ctx 读取
	runCtx := expand.WithPageSearchState(ctx, pageText, refMat)
	// DetachedCancel:取消信号不随 SSE 连接关闭传播,保证正常收尾落库不被断连打断;
	// 主动停止改由 /api/cancel -> managed.Cancel(requestID) 触发(见 cancelHandler)。
	requestID := uuid.NewString()

	// recall 状态注入（recall_history 工具）：会话定位 + 当前 scope + 各摘要
	// boundary CutoffAt（目录"摘要已覆盖"列判定用）。首轮会话尚未落库时
	// cutoffs 为空，全部标记未覆盖（降级安全）。
	runCtx = recall.WithRecallState(runCtx, sessionSvc, recallKey, filterKey,
		sessionSvc.SummaryCutoffs(ctx, recallKey))
	runOpts := []agent.RunOption{
		agent.WithRequestID(requestID),
		agent.WithDetachedCancel(true),
		agent.WithMaxRunDuration(maxRunDuration),
	}
	// 恒打标（文章哈希或 kind.ScopeGeneral）：事件落库自动继承、摘要按 scope 生成/注入。
	// 双写日志：标识（机读）+ 可读网址（人读，映射表有账可查）
	runOpts = append(runOpts, agent.WithEventFilterKey(filterKey))
	log.Infof("[Absorb] summary scope filterKey=%q url=%q (session=%s)",
		filterKey, scopeReadable(scopeURL), sessionID)
	// 工作集 L1：全局最近完整问答轮次经 WithLateContextMessages 注入
	// （不落库、逐 run）。开关关闭 / 读取失败 / 无可注入轮次时均不阻断主问答，
	// 仅记日志降级。
	var wsCfg conf.WorkingSetConfig
	if deps.WSFn != nil {
		wsCfg = deps.WSFn()
	}
	if wsCfg.On() {
		rounds, rerr := sessionSvc.RecentGlobalRounds(ctx, recallKey, wsCfg.L1Rounds())
		if rerr != nil {
			log.Warnf("[Absorb] working-set L1 read rounds failed, degraded (session=%s): %v", sessionID, rerr)
		} else {
			// 缩略图跨轮注入按默认模型视觉能力门控（与 B1 软闸同数据源）：非视觉
			// 模型不注入 image part。openai 层 WithTextOnlyMessageContent 本就
			// 兜底剥离，此处为防御层 + 省去无谓负载；读取失败按保守方向关闭。
			keepImages := false
			if mcCaps, merr := sessionSvc.GetDefaultModelConfig(ctx, app); merr == nil && mcCaps != nil {
				keepImages = mcCaps.ImageSupport
			}
			l1Msgs, stats, berr := buildWorkingSetL1(rounds, filterKey, wsCfg.L1Tokens(), keepImages)
			if berr != nil {
				// 超预算：可读报错终止本轮，不降级裁剪
				log.Warnf("[Absorb] working-set L1 over budget (session=%s): %v", sessionID, berr)
				return nil, berr
			}
			if len(l1Msgs) > 0 {
				runOpts = append(runOpts, agent.WithLateContextMessages(l1Msgs))
			}
			// 选取/保留/去重轮数留痕；kept=0（全去重或无轮次）即本轮未注入
			log.Infof("[Absorb] working-set L1 | selected=%d kept=%d dropped_dup=%d msgs=%d est_tokens=%d path=late_context (session=%s)",
				stats.Selected, stats.Kept, stats.DroppedDup, stats.Messages, stats.EstTokens, sessionID)
		}
	}
	return &PreparedInvocation{
		SessionID: sessionID,
		RequestID: requestID,
		FilterKey: filterKey,
		ScopeURL:  scopeReadable(scopeURL),
		Message:   msg,
		RunCtx:    runCtx,
		RunOpts:   runOpts,
	}, nil
}
