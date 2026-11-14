package chat

import (
	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"os"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/memory"
)

// AskContext 划词提问上下文（由 askReq 转换而来，不含会话状态）
type AskContext struct {
	SelectedText      string // 用户划选的句子（首轮非空，追问为空）
	Paragraph         string // 所在段落上下文（可空）
	PageContext       string // 页面级上下文：标题/标题路径/meta 描述（可空），锚定术语所属领域
	URL               string // 来源 URL
	PageTitle         string // 当前浏览页标题（无划词提问时随 URL 注入，可空）
	PageMd            string // 当前页整页结构化文本（无划词提问时扩展端采集，非空则注入 <page_content> 并免 fetch_page）
	DocumentID        string // /api/page/prepare 产物（PDF 解读）：非空表示 PageMd 为文档正文（划词提问也注入 <page_content>）
	ContentStatus     string // 页面正文完整度（full/partial/selected_only/unavailable，空=不上报不渲染）
	Question          string // 用户提问
	ReferenceMaterial string // 会话参考材料：首轮=SelectedText，追问=从历史查得（追问重申机制）
}

// PromptStrategy 按模型族差异化的提示词构建策略。
// 仅负责文本构建（system instruction + user message），不管 LLM 调用、SSE 编码、会话状态。
type PromptStrategy interface {
	// ID 返回策略标识（= variant），用于日志区分。
	ID() string
	// SystemInstruction 返回该模型最佳的 system prompt（角色+指令+约束）。
	SystemInstruction() string
	// BuildUserMessage 把划词上下文+问题拼成 user message。
	BuildUserMessage(ctx AskContext) string
}

// 策略注册表：variant → 构造函数。未来加新模型只需在此注册一项；
// 未注册的 variant 复用 GLM 提示词（不再维护第二套默认策略）。
// system prompt 编译期内嵌（见 glmSystemPrompt），不落盘、不读外部文件（防篡改/防翻看）。
var strategyRegistry = map[string]func() (PromptStrategy, error){
	string(kind.VariantGLM): newGLMStrategy,
}

// PromptOption 策略构造可选参数。
type PromptOption func(*promptOptions)

type promptOptions struct {
	profilePath string      // L3 用户画像文件路径；空=不注入
	memGate     *MemoryGate // 记忆运行时开关；非 nil 时关闭态不注入画像（热切换）
}

// WithProfilePath 设置用户画像文件路径（存在时注入 <user_profile> 块）。
func WithProfilePath(path string) PromptOption {
	return func(o *promptOptions) { o.profilePath = path }
}

// WithProfileGate 注入记忆运行时开关：记忆关闭时不注入 <user_profile>（设置页热切换）。
func WithProfileGate(gate *MemoryGate) PromptOption {
	return func(o *promptOptions) { o.memGate = gate }
}

// NewPromptStrategy 按 variant 选策略；未注册 variant 复用 GLM 提示词，保留原名用于日志。
func NewPromptStrategy(variant string, opts ...PromptOption) (PromptStrategy, error) {
	o := promptOptions{}
	for _, fn := range opts {
		fn(&o)
	}
	var ps PromptStrategy
	if fn, ok := strategyRegistry[strings.ToLower(variant)]; ok {
		var err error
		ps, err = fn()
		if err != nil {
			return nil, err
		}
	} else {
		// 未注册 variant 默认复用 GLM 提示词（单一真相源，不再维护第二套默认提示词）；
		// variant 仅保留在 ID() 与日志里，不改变行为。
		ps = newGLMStrategyWithVariant(variant)
	}
	if sp, ok := ps.(interface{ setProfilePath(string) }); ok {
		sp.setProfilePath(o.profilePath)
	}
	if sp, ok := ps.(interface{ setMemoryGate(*MemoryGate) }); ok {
		sp.setMemoryGate(o.memGate)
	}
	return ps, nil
}

// ---------- GLM 策略 ----------
// 依据智谱官方提示词规范与 GLM 模型特性：
//   - 强指令放 system 与 user message 开头（GLM 对开头指令更敏感）
//   - 显式角色 + 指定中文
//   - 防幻觉约束 + 输出格式约束
//   - 结构化标签 <reference_material>/<paragraph_context> 分隔参考材料（支持多源上下文）
//   - few-shot 调用示例引导工具触发
type glmStrategy struct {
	variant      string        // 策略标识（未注册 variant 复用 GLM 提示词时保留原名，仅用于日志）
	systemPrompt string        // newGLMStrategy 时由内嵌资源注入
	profilePath  string        // L3 用户画像文件路径（空=不注入）
	memGate      *MemoryGate   // 记忆运行时开关（nil=不做门控，画像始终按文件注入）
	memReader    memory.Reader // L1 记忆检索（NewRunner 装配；nil=eval 等工具链，不注入相关记忆块）
}

func (s *glmStrategy) setProfilePath(p string)         { s.profilePath = p }
func (s *glmStrategy) setMemoryGate(g *MemoryGate)     { s.memGate = g }
func (s *glmStrategy) setMemoryReader(r memory.Reader) { s.memReader = r }

// promptFS 内嵌提示词目录（prompts/*.md），编译进二进制不落盘（防明文暴露）。
// 新增专用提示词策略 = 放一个 prompts/<variant>-system.md 并在注册表登记；
// 不登记则默认复用 glm-system.md，无需新增 embed 指令。
//
//go:embed prompts
var promptFS embed.FS

// mustPrompt 读取内嵌提示词文件；缺失属编译期契约破坏，直接 panic 暴露。
func mustPrompt(name string) string {
	b, err := promptFS.ReadFile("prompts/" + name)
	if err != nil {
		panic(fmt.Sprintf("chat: 内嵌提示词 %s 缺失: %v", name, err))
	}
	return string(b)
}

// glmSystemPrompt GLM 系统提示词（prompts/glm-system.md）。
var glmSystemPrompt = mustPrompt("glm-system.md")

func newGLMStrategy() (PromptStrategy, error) {
	return newGLMStrategyWithVariant(string(kind.VariantGLM)), nil
}

// newGLMStrategyWithVariant 构造使用 GLM 提示词、但保留指定 variant 名的策略。
// 用于未注册 variant 的默认路径：提示词只有一份，variant 仅影响 ID() 与日志。
func newGLMStrategyWithVariant(variant string) *glmStrategy {
	return &glmStrategy{variant: variant, systemPrompt: glmSystemPrompt}
}

// PromptDigest 返回内嵌 GLM 系统提示词的指纹（sha256 前 8 位十六进制 + 字节数），
// 供启动日志确认运行中的提示词版本。
func PromptDigest() string {
	sum := sha256.Sum256([]byte(glmSystemPrompt))
	return fmt.Sprintf("%x/%dB", sum[:8], len(glmSystemPrompt))
}

// maxProfileRunes 画像单次注入的字符上限（蒸馏侧 maxTokens 已隐式限长，此为读取侧兜底）。
const maxProfileRunes = 4000

func (s *glmStrategy) ID() string {
	if s.variant == "" {
		return string(kind.VariantGLM)
	}
	return s.variant
}

func (s *glmStrategy) SystemInstruction() string {
	return s.systemPrompt
}

// BuildUserMessage 把划词上下文+问题拼成 user message，顶部依次注入：
// L3 用户画像块（文件由后台蒸馏器周期更新，每请求动态读取 → 蒸馏后立即生效）
// + L1 相关记忆块（按当前问题检索，服务端预检索替代框架预加载）。两者都在
// 当前轮 user 消息 = 前缀缓存安全区，历史中的旧块文本冻结不受影响。
func (s *glmStrategy) BuildUserMessage(c AskContext) string {
	return s.userProfileBlock() + s.relatedMemoriesBlock(c) + s.buildUserMessageBase(c)
}

// relatedMemories 预算常量：条数上限 / 检索超时 / 块内总字符兜底（与画像读取侧截断同思路）。
const (
	relatedMemoryMaxResults = 6
	relatedMemoryTimeout    = 1500 * time.Millisecond
	relatedMemoryMaxRunes   = 1500
)

// relatedMemoriesBlock 按当前问题检索 L1 相关记忆拼 <related_memories> 块。
// 跨轮稳定前缀 + 每轮检索本条消息尾部：相关记忆随问题变化不破坏前缀缓存。
// 检索失败或空结果时回退最近记忆（与框架预加载同语义），仍为空/无 reader（eval
// 工具链）/记忆门控关闭/问题为空则整体不注入（零风险降级；门控关闭态由
// gatedMemoryService 短路返回空结果）。
func (s *glmStrategy) relatedMemoriesBlock(c AskContext) string {
	if s.memReader == nil || strings.TrimSpace(c.Question) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), relatedMemoryTimeout)
	defer cancel()
	userKey := memory.UserKey{AppName: AppName, UserID: UserID}
	entries, err := s.memReader.SearchMemories(ctx, userKey, c.Question,
		memory.WithSearchOptions(memory.SearchOptions{MaxResults: relatedMemoryMaxResults}))
	if err != nil || len(entries) == 0 {
		entries, err = s.memReader.ReadMemories(ctx, userKey, relatedMemoryMaxResults)
	}
	if err != nil {
		log.Debugf("[Absorb] related memories load failed, skip injection: %v", err)
		return ""
	}
	var b strings.Builder
	lines := make([]string, 0, len(entries))
	budget := relatedMemoryMaxRunes
	for _, e := range entries {
		if e == nil || e.Memory == nil || strings.TrimSpace(e.Memory.Memory) == "" {
			continue
		}
		line := "- " + strings.TrimSpace(e.Memory.Memory) + "\n"
		r := []rune(line)
		if len(r) > budget {
			break
		}
		lines = append(lines, line)
		budget -= len(r)
	}
	if len(lines) == 0 {
		return ""
	}
	b.WriteString("<related_memories>\n")
	b.WriteString("以下是按当前问题检索出的用户历史学习记忆（既往主题/结论/偏好），仅用于对比、关联与延续既往学习，不是本轮事实依据：\n")
	for _, line := range lines {
		b.WriteString(line)
	}
	b.WriteString("</related_memories>\n\n")
	return b.String()
}

// userProfileBlock 读取画像文件拼 <user_profile> 块；文件缺失/为空或记忆已关闭时
// 返回空串（零风险降级，关闭态随设置页热切换即时生效）。超长截断防上下文侵蚀。
func (s *glmStrategy) userProfileBlock() string {
	if s.profilePath == "" {
		return ""
	}
	if s.memGate != nil && !s.memGate.Enabled() {
		return ""
	}
	b, err := os.ReadFile(s.profilePath)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return ""
	}
	text := string(b)
	if runes := []rune(text); len(runes) > maxProfileRunes {
		text = string(runes[:maxProfileRunes])
		log.Warnf("[Absorb] profile.md 超过 %d 字符，已截断注入（实际 %d）", maxProfileRunes, len(runes))
	}
	return "<user_profile>\n" + text + "\n</user_profile>\n\n"
}

// pageContentHeadRunes <page_content> 注入的头部预算：首屏/导语通常在此范围内，
// 全量内容已入页面缓存，更多细节由模型按需 expand_context 检索（避免 prompt 膨胀）。
const pageContentHeadRunes = 6000

// pageContentBlock 扩展端采集到页面内容时注入 <page_content>：LLM 直接看到正文，
// 无需再调 fetch_page（对 SPA/内网页面，HTTP 抓取本就拿不到渲染态内容）。
// 网页划词时不注入——划词即来源于页面正文，页面全文与参考材料冗余；
// 文档划词（DocumentID 非空）例外：划词只是文档一小段，全文才是回答关键
// （实测 flash 模型不会主动调 expand_context，正文不在场就答"不知道"）。
func pageContentBlock(c AskContext) string {
	if strings.TrimSpace(c.PageMd) == "" {
		return ""
	}
	if strings.TrimSpace(c.SelectedText) != "" && strings.TrimSpace(c.DocumentID) == "" {
		return ""
	}
	md := []rune(strings.TrimSpace(c.PageMd))
	tail := ""
	if len(md) > pageContentHeadRunes {
		md = md[:pageContentHeadRunes]
		tail = "\n（以上为页面开头部分；完整内容已缓存，如需更多细节可调用 expand_context 工具按关键词检索）"
	}
	var b strings.Builder
	b.WriteString("<page_content>\n")
	b.WriteString("来源：" + c.URL + "\n\n")
	b.WriteString(string(md))
	b.WriteString(tail)
	b.WriteString("\n</page_content>\n\n")
	return b.String()
}

// pageURLBlock 无划词提问时注入当前浏览页 URL，提示模型按需 fetch_page。
// 有划词时不注入：<page_url> 已被"划词来源页"占用，同名标签每条消息最多出现一个；
// 扩展端已采集到页面内容（PageMd 非空）时也不注入：<page_content> 已在场，无需 fetch_page。
func pageURLBlock(c AskContext) string {
	if strings.TrimSpace(c.SelectedText) != "" || strings.TrimSpace(c.URL) == "" || strings.TrimSpace(c.PageMd) != "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("<page_url>\n")
	b.WriteString(c.URL)
	if strings.TrimSpace(c.PageTitle) != "" {
		b.WriteString("\n（用户当前正在浏览此页面，标题：" + c.PageTitle + "）")
	}
	b.WriteString("\n</page_url>\n\n")
	b.WriteString("若回答该问题需要页面正文，请先调用 fetch_page 工具获取此 URL 的内容后再回答；若凭标题或问题即可回答，无需调用。\n\n")
	return b.String()
}

// contentStatusBlock 页面正文完整度状态块：宿主上报 ContentStatus 时注入，
// 让模型明确知道材料边界（partial/selected_only/unavailable 时不得假装读过全文）。
// 空值不渲染，生产端未上报（扩展端尚未透传）时提示词与历史行为完全一致。
func contentStatusBlock(c AskContext) string {
	status := strings.TrimSpace(c.ContentStatus)
	if status == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("<page_content_status>" + status + "</page_content_status>\n")
	b.WriteString("（页面正文完整度说明：full=已获得完整正文；partial=仅有部分正文；selected_only=只有划选文本；unavailable=没有可用正文。材料不完整时不得假装已读过全文，涉及未读内容须明确说明材料缺失。）\n\n")
	return b.String()
}

func (s *glmStrategy) buildUserMessageBase(c AskContext) string {
	var b strings.Builder
	// 首轮（带新划词）：完整模板，user message 开头放强指令利用 GLM 的开头偏置
	// 调性与 system prompt 一致：引导补充背景与类比，而非限制"严禁编造"
	if strings.TrimSpace(c.SelectedText) != "" {
		b.WriteString("请基于以下材料解释问题，可补充背景知识、类比或举例帮助理解。\n\n")
		b.WriteString(contentStatusBlock(c))
		// 页面级上下文优先注入：标题/章节路径锚定术语所属领域，消除通用词歧义
		if strings.TrimSpace(c.PageContext) != "" {
			b.WriteString("<page_context>\n")
			b.WriteString(c.PageContext)
			b.WriteString("\n</page_context>\n\n")
		}
		if strings.TrimSpace(c.URL) != "" {
			b.WriteString("<page_url>\n")
			b.WriteString(c.URL)
			b.WriteString("\n</page_url>\n\n")
		}
		// 文档场景（DocumentID 非空）划词也注入正文：划词只是文档一小段，全文才是
		// 回答关键（网页划词由 pageContentBlock 内部跳过，维持原冗余规避）
		b.WriteString(pageContentBlock(c))
		b.WriteString("<reference_material>\n")
		b.WriteString(c.SelectedText)
		b.WriteString("\n</reference_material>\n\n")
		if strings.TrimSpace(c.Paragraph) != "" {
			b.WriteString("<paragraph_context>\n")
			b.WriteString(c.Paragraph)
			b.WriteString("\n</paragraph_context>\n\n")
		}
		b.WriteString("问题：")
		b.WriteString(c.Question)
		return b.String()
	}
	// 追问（无新划词）：重申会话参考材料，让 flash 模型在最新 user message 尾部看到材料
	if strings.TrimSpace(c.ReferenceMaterial) != "" {
		b.WriteString(contentStatusBlock(c))
		b.WriteString(pageContentBlock(c))
		b.WriteString(pageURLBlock(c))
		b.WriteString("<reference_material>\n")
		b.WriteString(c.ReferenceMaterial)
		b.WriteString("\n</reference_material>\n\n")
		b.WriteString("问题：")
		b.WriteString(c.Question)
		return b.String()
	}
	// 无任何参考材料：直接发问题（可能带当前浏览页内容或 URL）
	b.WriteString(contentStatusBlock(c))
	b.WriteString(pageContentBlock(c))
	b.WriteString(pageURLBlock(c))
	b.WriteString("问题：")
	b.WriteString(c.Question)
	return b.String()
}
