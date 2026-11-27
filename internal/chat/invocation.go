package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/skill"

	retriever "absorb-agent/tool/pageretriever"
	readpage "absorb-agent/tool/read_page"
	recall "absorb-agent/tool/recall_history"
	searchdoc "absorb-agent/tool/search_document"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
	"absorb-agent/internal/retrieval"
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
	Text          string   `json:"text"`
	QuoteTexts    []string `json:"quoteTexts"` // 多引用全部文本（含第一条，序号=chip 顺序；空=旧扩展单引用）
	Paragraph     string   `json:"paragraph"`
	PageContext   string   `json:"pageContext"`
	URL           string   `json:"url"`
	PageMd        string   `json:"pageMd"`        // 整页结构化文本（扩展端已转换，直读入缓存，上限 100K 字符）
	ContentStatus string   `json:"contentStatus"` // 页面正文完整度：full/partial/selected_only/unavailable（空=不上报，提示词不渲染）
}

// PageInput 当前浏览页：无划词提问时供 agent fetch_page 获取正文。
// DocumentID 为 /api/page/prepare 的产物（documentId = documents.id UUID，
// M0 前为 URL，问答链路按 ID 优先、URL 回退），
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

// stableSkillsOverview 技能概览的稳定渲染：仅技能名+描述清单（按名排序）。
// 不含默认渲染里的逐请求推荐行与路由提示，保证 system message 跨请求字节级一致。
func stableSkillsOverview(_ context.Context, req agent.AvailableSkillsRenderRequest) string {
	if len(req.Summaries) == 0 {
		return ""
	}
	sums := append([]skill.Summary(nil), req.Summaries...)
	sort.Slice(sums, func(i, j int) bool { return sums[i].Name < sums[j].Name })
	var b strings.Builder
	fmt.Fprintf(&b, "Available skills (%d):\n", len(sums))
	for _, s := range sums {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return b.String()
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

// newRunOpts 每轮 run 的公共选项：请求标识（/api/cancel 按其定位 run）、取消
// 信号与 SSE 连接解耦（断连不影响收尾落库）、run 最长存活时间。轮次专属选项
// （scope 打标 / 技能渲染 / 恢复标记）由调用方经 extra 按原顺序追加。
func newRunOpts(requestID string, extra ...agent.RunOption) []agent.RunOption {
	opts := []agent.RunOption{
		agent.WithRequestID(requestID),
		agent.WithDetachedCancel(true),
		agent.WithMaxRunDuration(maxRunDuration),
	}
	return append(opts, extra...)
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
		askCtx.QuoteTexts = in.Context.QuoteTexts
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
	// 查看器地址安全网：划词 URL 是浏览器内置查看器的扩展页地址（采集侧纠正
	// 未生效的存量数据）且当前浏览页为正常网页时，改用浏览页 URL——来源行渲染、
	// 下方 scope 归组随之纠正（须在 scopeURL 计算前落位）
	if in.Page != nil && isViewerURL(askCtx.URL) && isHTTPURL(in.Page.URL) {
		log.Infof("[Absorb] quote url is viewer page, fallback to page url: %s", scopeReadable(in.Page.URL))
		askCtx.URL = in.Page.URL
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

	// 页面文本缓存（read_page 章节索引与页文本来源，scope 感知：同会话多页面
	// 各存一行）：带新划词 → pageMd（扩展端已转换，免转换直读）写入该 scope
	// 缓存；无新划词（追问）/pageMd 为空（转换失败等）→ 按当前 scope 读缓存，
	// 查无则空（宁缺勿错，不串其他页面）；都无 → read_page 返回无缓存失败。
	// 文档轮（页面携带 documentId）不读写页面缓存：页面缓存为网页划词专属，
	// 文档场景的证据检索与原文阅读走 search_document（读页模式自行加载
	// document_contents）。
	isDocTurn := in.Page != nil && strings.TrimSpace(in.Page.DocumentID) != ""
	pageText := ""
	if !isDocTurn {
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
	}
	// 文档上下文（PDF 解读 PR2）：页面携带 documentId（/api/page/prepare 产物，
	// M0 起 = documents.id UUID）时注入已提取正文。<page_content> 渲染取该字段
	// 头部 6000 字，全量阅读走 search_document 读页模式（不进 expand 页面缓存）。
	// 兼容窗口：M0 前扩展端卡片持 URL 语义，按 ID 查无时回退按 URL 读（宁缺勿错
	// 原则不变，回退仅一次查询）。
	if in.Page != nil {
		if docID := strings.TrimSpace(in.Page.DocumentID); docID != "" {
			dc, derr := sessionSvc.LoadDocumentContentByID(ctx, app, user, docID)
			if dc == nil && derr == nil {
				// 旧语义兼容：documentId 可能是 M0 前的 URL
				dc, derr = sessionSvc.LoadDocumentContent(ctx, app, user, docID)
			}
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
				// 回写 askCtx.PageMd：prompt 侧 <page_content> 只从该字段渲染
				// （pageContentBlock）。不回写则模型看不到正文在场，还会被
				// <page_url> 的"按需 fetch_page"提示误导（正文已在手，fetch 徒劳）。
				// 截断后 ≤ PageTextMaxLen，pageContentBlock 再取头部 6000 字注入。
				askCtx.PageMd = retriever.TruncatePageText(dc.ContentText)
				// 标记文档场景：划词提问也注入 <page_content>
				askCtx.DocumentID = docID
				// 材料形态自标（kind.DocType）：prompt 侧来源行标注与工具引导分叉依据
				askCtx.DocType = docTypeOf(dc.ContentType, dc.URL)
				log.Infof("[Absorb] document content injected (doc=%s status=%s stored=%d chars injected<=%d)",
					docID, dc.ExtractionStatus, dc.CharCount, retriever.PageTextMaxLen)
			}
		}
	}
	prompt := ps.BuildUserMessage(askCtx)
	log.Debugf("[Absorb] [%s] prompt:\n%s\n---", ps.ID(), prompt)
	msg := model.NewUserMessage(prompt)
	for _, im := range in.Images { // 文本 prompt 不变（前端已带“请解读图片内容”），仅追加图片 part
		msg.AddImageURL(imageSource(im), "auto") // src 为 base64 或 http(s) 链接均兼容
	}
	// 文档检索域（M3 hybrid）：当前文档锚定注入（不接受模型指定 document_id，
	// 范围与归属校验都在服务端）；无文档轮为空 = 租户内全部已索引文档。
	var docScope []string
	if askCtx.DocumentID != "" {
		docScope = []string{askCtx.DocumentID}
	}
	// 工具运行态注入（read_page / search_document / recall_history 三件套），
	// 与恢复轮 resumePhase 共用同一实现，保证两轮工具看到同一状态口径。
	runCtx := injectToolState(ctx, sessionSvc, app, user, sessionID, filterKey, pageText, docScope)
	// DetachedCancel:取消信号不随 SSE 连接关闭传播,保证正常收尾落库不被断连打断;
	// 主动停止改由 /api/cancel -> managed.Cancel(requestID) 触发(见 cancelHandler)。
	requestID := uuid.NewString()
	runOpts := newRunOpts(requestID,
		// 技能概览用稳定渲染：默认渲染带"Top recommended skill for this request"
		// 逐请求推荐行，且整个概览被 prepend 到 system message 最前面，推荐行
		// 一变前缀缓存就在 system 头部失效（首个消息之后的全部内容不计缓存）。
		agent.WithAvailableSkillsRenderer(stableSkillsOverview))
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
		rounds, rerr := sessionSvc.RecentGlobalRounds(ctx,
			session.Key{AppName: app, UserID: user, SessionID: sessionID}, wsCfg.L1Rounds())
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
			l1Msgs, stats := buildWorkingSetL1(rounds, filterKey, wsCfg.L1Tokens(), keepImages)
			if stats.TrimmedOld > 0 {
				// 超预算降级留痕：从最旧轮剔除直至入限（确定性裁法，可继续提问，无窗口死锁）
				log.Warnf("[Absorb] working-set L1 over budget, trimmed oldest %d round(s), kept est_tokens=%d (session=%s)",
					stats.TrimmedOld, stats.EstTokens, sessionID)
			}
			if len(l1Msgs) > 0 {
				runOpts = append(runOpts, agent.WithLateContextMessages(l1Msgs))
			}
			// 选取/保留/去重/降级轮数留痕；kept=0（全去重或无轮次）即本轮未注入
			log.Infof("[Absorb] working-set L1 | selected=%d kept=%d dropped_dup=%d trimmed_old=%d msgs=%d est_tokens=%d path=late_context (session=%s)",
				stats.Selected, stats.Kept, stats.DroppedDup, stats.TrimmedOld, stats.Messages, stats.EstTokens, sessionID)
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

// injectToolState 注入三个工具的运行态上下文（生产组装轮与恢复轮共用）：
//   - read_page：页面章节索引与页文本（WithPageSectionState）
//   - search_document：文档检索状态，docScope 非空 = 锚定当前文档（检索与归属
//     校验都在服务端），为空 = 租户内全部已索引文档；SharedEmbedder 为 nil
//     （未配置）时自动纯词法
//   - recall_history：会话定位 + 当前 scope + 各摘要 boundary CutoffAt（目录
//     "摘要已覆盖"列判定用）。首轮会话尚未落库时 cutoffs 为空，全部标记未覆盖
//     （降级安全）。
func injectToolState(ctx context.Context, sessionSvc *store.SessionService, app, user, sessionID, filterKey, pageText string, docScope []string) context.Context {
	runCtx := readpage.WithPageSectionState(ctx, pageText)
	runCtx = searchdoc.WithDocumentQueryState(runCtx, sessionSvc.Storage(),
		retrieval.SharedEmbedder(), sessionSvc.Storage(), sessionSvc.Storage(), app, user, docScope)
	recallKey := session.Key{AppName: app, UserID: user, SessionID: sessionID}
	return recall.WithRecallState(runCtx, sessionSvc, recallKey, filterKey,
		sessionSvc.SummaryCutoffs(ctx, recallKey))
}

// isViewerURL 报告 u 是否为浏览器内置查看器的扩展页地址（Chrome/Edge 内置 PDF
// 查看器内划词时 grab 自报的 location.href），此类地址不参与来源渲染与 scope 归组。
func isViewerURL(u string) bool {
	return strings.HasPrefix(u, "chrome-extension://") || strings.HasPrefix(u, "moz-extension://")
}

// isHTTPURL 报告 u 是否为正常网页地址（http/https）。
func isHTTPURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// docTypeOf 服务端自查材料形态：ContentType 权威（服务端按 content-type 兜底分类），
// URL 后缀兜底；非 PDF 落网页口径（DocTypeWeb）。未知格式未来加枚举项再扩。
func docTypeOf(contentType, url string) kind.DocType {
	if strings.EqualFold(strings.TrimSpace(contentType), "application/pdf") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(url)), ".pdf") {
		return kind.DocTypePDF
	}
	return kind.DocTypeWeb
}
