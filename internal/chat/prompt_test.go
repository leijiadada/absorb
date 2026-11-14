package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/memory"
)

// ===== GLM 策略 =====

func TestGLMStrategy_ID(t *testing.T) {
	s := &glmStrategy{}
	if got := s.ID(); got != "glm" {
		t.Errorf("ID() = %q, want %q", got, "glm")
	}
}

// newTestGLMStrategy 构造指定内容的 GLM 策略（system prompt 已内嵌，测试直接注入字段）。
func newTestGLMStrategy(content string) *glmStrategy {
	return &glmStrategy{systemPrompt: content}
}

func TestGLMStrategy_SystemInstruction(t *testing.T) {
	testPrompt := "你是阅读助手。必须基于参考材料回答，严禁编造材料之外的内容。用中文回答，分点回答。"
	s := newTestGLMStrategy(testPrompt)
	instr := s.SystemInstruction()
	cases := map[string]string{
		"显式角色":   "你是阅读助手",
		"强指令-必须": "必须",
		"强指令-严禁": "严禁",
		"指定中文":   "用中文回答",
		"输出格式":   "分点",
		"防幻觉":    "材料之外",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(instr, want) {
				t.Errorf("SystemInstruction 缺少%s，期望包含 %q。全文: %q", name, want, instr)
			}
		})
	}
}

func TestGLMStrategy_SystemInstruction_PassesThroughContent(t *testing.T) {
	testPrompt := "MARKER_UNIQUE_CONTENT_12345"
	s := newTestGLMStrategy(testPrompt)
	if got := s.SystemInstruction(); got != testPrompt {
		t.Errorf("SystemInstruction 应完整回传内容，got %q want %q", got, testPrompt)
	}
}

func TestGLMStrategy_newGLMStrategy_EmbeddedPromptLoaded(t *testing.T) {
	s, err := newGLMStrategy()
	if err != nil {
		t.Fatalf("newGLMStrategy 失败: %v", err)
	}
	if strings.TrimSpace(s.SystemInstruction()) == "" {
		t.Errorf("内嵌 system prompt 不应为空")
	}
}

func TestGLMStrategy_BuildUserMessage_WithParagraph(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{
		SelectedText: "区块链",
		Paragraph:    "区块链是一种分布式账本技术，通过密码学保证数据不可篡改。",
		PageContext:  "页面标题: 区块链入门指南\n所在章节: 第一章 > 1.2 核心概念",
		URL:          "https://example.com/a",
		Question:     "这个词什么意思",
	}
	got := s.BuildUserMessage(ctx)

	// 开头强指令（利用 GLM 开头偏置），调性为"解释"而非"严禁编造"
	if !strings.HasPrefix(got, "请基于以下材料解释问题") {
		t.Errorf("user message 应以引导型强指令开头，got: %q", got[:min(len(got), 40)])
	}
	// 结构化标签分隔（替代三引号）
	if !strings.Contains(got, "<reference_material>") || !strings.Contains(got, "</reference_material>") {
		t.Errorf("期望包含 <reference_material> 标签对。got: %q", got)
	}
	// 页面级上下文与 URL 注入（锚定术语所属领域）
	if !strings.Contains(got, "<page_context>") || !strings.Contains(got, "区块链入门指南") {
		t.Errorf("期望包含 <page_context> 标签及页面标题。got: %q", got)
	}
	if !strings.Contains(got, "<page_url>") || !strings.Contains(got, "https://example.com/a") {
		t.Errorf("期望包含 <page_url> 标签及 URL。got: %q", got)
	}
	if !strings.Contains(got, "<paragraph_context>") || !strings.Contains(got, "</paragraph_context>") {
		t.Errorf("期望包含 <paragraph_context> 标签对。got: %q", got)
	}
	// 不应再出现三引号
	if strings.Contains(got, `"""`) {
		t.Errorf("不应再使用三引号分隔。got: %q", got)
	}
	for _, want := range []string{"区块链", "分布式账本", "这个词什么意思", "问题："} {
		if !strings.Contains(got, want) {
			t.Errorf("BuildUserMessage 缺少 %q。全文: %q", want, got)
		}
	}
}

func TestGLMStrategy_BuildUserMessage_FollowUpWithReferenceMaterial(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{
		ReferenceMaterial: "梯度下降是优化算法",
		Question:          "它和随机梯度下降有什么区别",
	}
	got := s.BuildUserMessage(ctx)

	if !strings.Contains(got, "<reference_material>") || !strings.Contains(got, "</reference_material>") {
		t.Errorf("追问有 ReferenceMaterial 时应注入 <reference_material> 标签。got: %q", got)
	}
	if !strings.Contains(got, "梯度下降是优化算法") {
		t.Errorf("应包含重申的参考材料内容。got: %q", got)
	}
	if !strings.Contains(got, "问题：它和随机梯度下降有什么区别") {
		t.Errorf("应包含问题。got: %q", got)
	}
	if strings.HasPrefix(got, "请基于以下材料") {
		t.Errorf("追问不应包含首轮强指令。got: %q", got)
	}
	if strings.Contains(got, "<paragraph_context>") {
		t.Errorf("追问不应包含 <paragraph_context>。got: %q", got)
	}
}

func TestGLMStrategy_BuildUserMessage_NoParagraph(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{
		SelectedText: "Change Buffer",
		Question:     "这是什么",
	}
	got := s.BuildUserMessage(ctx)

	if strings.Contains(got, "<paragraph_context>") {
		t.Errorf("无段落时不应包含 <paragraph_context> 块。got: %q", got)
	}
	if strings.Contains(got, "<page_context>") || strings.Contains(got, "<page_url>") {
		t.Errorf("无页面上下文/URL 时不应包含 <page_context>/<page_url> 块。got: %q", got)
	}
	for _, want := range []string{"Change Buffer", "这是什么", "<reference_material>", "</reference_material>", "问题："} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q。got: %q", want, got)
		}
	}
}

func TestGLMStrategy_BuildUserMessage_EmptyContext(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{Question: "你好"}
	got := s.BuildUserMessage(ctx)
	if !strings.Contains(got, "问题：你好") {
		t.Errorf("空上下文仍应拼接问题。got: %q", got)
	}
	if strings.Contains(got, "<reference_material>") {
		t.Errorf("空上下文不应包含 <reference_material> 块。got: %q", got)
	}
}

// ===== 可视化解释规则（默认策略与 GLM 同一份提示词） =====

// 默认路径与 GLM 共用 glm-system.md，都必须携带可视化规则关键内容：
// 触发规则与最小 DSL 骨架，缺了会导致命中场景不输出可视化页面。
func TestPromptStrategy_VisualRuleInDefaultPath(t *testing.T) {
	strategies := map[string]string{}
	for _, v := range []string{"glm", "deepseek"} {
		ps, err := NewPromptStrategy(v)
		if err != nil {
			t.Fatalf("NewPromptStrategy(%s): %v", v, err)
		}
		strategies[v] = ps.SystemInstruction()
	}
	for name, instr := range strategies {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				":::absorb-visual",      // 容器语法
				"```callout",            // 组件围栏写法（上游主语法，推荐）
				"```chart bar",          // chart 组件围栏写法
				"template: sheet",       // frontmatter 骨架
				"## 面板",                 // 分节结构
				"简单问答禁止使用",              // 触发白名单反例
				"visual-explanation 技能", // 优先加载技能
			} {
				if !strings.Contains(instr, want) {
					t.Errorf("%s 策略缺少可视化规则要素 %q", name, want)
				}
			}
		})
	}
}

// ===== 当前浏览页 URL 注入 =====

func TestGLMStrategy_BuildUserMessage_PageURL_NoQuote(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{
		URL:       "https://example.com/article",
		PageTitle: "流处理与概率方法",
		Question:  "这篇文章主要讲了什么",
	}
	got := s.BuildUserMessage(ctx)

	if !strings.Contains(got, "<page_url>") || !strings.Contains(got, "https://example.com/article") {
		t.Errorf("无划词带 URL 时应注入 <page_url> 块。got: %q", got)
	}
	if !strings.Contains(got, "流处理与概率方法") {
		t.Errorf("应包含页面标题。got: %q", got)
	}
	if !strings.Contains(got, "fetch_page") {
		t.Errorf("应包含 fetch_page 调用提示。got: %q", got)
	}
	if !strings.Contains(got, "问题：这篇文章主要讲了什么") {
		t.Errorf("应包含问题。got: %q", got)
	}
	if n := strings.Count(got, "<page_url>"); n != 1 {
		t.Errorf("<page_url> 应只出现一次，got %d 次。got: %q", n, got)
	}
}

func TestGLMStrategy_BuildUserMessage_PageURL_FollowUpOrder(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{
		URL:               "https://example.com/article",
		ReferenceMaterial: "上轮划词材料",
		Question:          "再详细讲讲",
	}
	got := s.BuildUserMessage(ctx)

	// 追问带 page：page 块在前，材料重申在后（保持材料贴近问题尾部的 recency 偏置）
	if !strings.Contains(got, "<page_url>") {
		t.Errorf("追问带 URL 应注入 <page_url>。got: %q", got)
	}
	if !strings.Contains(got, "<reference_material>") {
		t.Errorf("追问仍应重申 <reference_material>。got: %q", got)
	}
	if strings.Index(got, "<page_url>") > strings.Index(got, "<reference_material>") {
		t.Errorf("<page_url> 应位于 <reference_material> 之前。got: %q", got)
	}
}

func TestGLMStrategy_BuildUserMessage_PageURL_SkippedWithQuote(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{
		SelectedText: "布隆过滤器",
		URL:          "https://example.com/a",
		PageTitle:    "当前浏览页标题",
		Question:     "什么意思",
	}
	got := s.BuildUserMessage(ctx)

	// 有划词：现有模板注入划词来源 <page_url>，当前页块不得重复注入
	if n := strings.Count(got, "<page_url>"); n != 1 {
		t.Errorf("有划词时 <page_url> 应只出现一次（划词来源），got %d 次。got: %q", n, got)
	}
	if strings.Contains(got, "当前正在浏览此页面") {
		t.Errorf("有划词时不应注入当前浏览页标注。got: %q", got)
	}
	if strings.Contains(got, "fetch_page") {
		t.Errorf("有划词时不应附加 fetch_page 调用提示。got: %q", got)
	}
}

func TestGLMStrategy_BuildUserMessage_PageURL_Absent(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{Question: "你好"}
	got := s.BuildUserMessage(ctx)
	if strings.Contains(got, "<page_url>") {
		t.Errorf("无 URL 时不应包含 <page_url>。got: %q", got)
	}
}

// ===== 工厂选择 =====

func TestNewPromptStrategy_GLM(t *testing.T) {
	ps, err := NewPromptStrategy("glm")
	if err != nil {
		t.Fatalf("glm variant 应成功: %v", err)
	}
	if ps.ID() != "glm" {
		t.Errorf("glm variant 应返回 glm 策略，got ID=%q", ps.ID())
	}
	if _, ok := ps.(*glmStrategy); !ok {
		t.Errorf("glm variant 应返回 *glmStrategy，got %T", ps)
	}
}

func TestNewPromptStrategy_UnknownVariant(t *testing.T) {
	// 未注册 variant 复用 GLM 提示词与 user message 模板，仅 ID 保留模型名
	ps, err := NewPromptStrategy("qwen")
	if err != nil {
		t.Errorf("未知 variant 不应报错: %v", err)
	}
	if ps.ID() != "qwen" {
		t.Errorf("未知 variant 应保留 variant 名，got ID=%q", ps.ID())
	}
	if _, ok := ps.(*glmStrategy); !ok {
		t.Errorf("未知 variant 应返回 *glmStrategy，got %T", ps)
	}
	// 提示词必须与 GLM 完全一致：默认路径没有第二套实现
	glm, err := NewPromptStrategy("glm")
	if err != nil {
		t.Fatalf("NewPromptStrategy(glm): %v", err)
	}
	if ps.SystemInstruction() != glm.SystemInstruction() {
		t.Error("未知 variant 应复用 GLM system prompt")
	}
	if got := ps.BuildUserMessage(AskContext{SelectedText: "缓存", Question: "解释下"}); !strings.HasPrefix(got, "请基于以下材料解释问题") {
		t.Errorf("未知 variant 应复用 GLM user message 模板。got: %q", got)
	}
}

func TestNewPromptStrategy_CaseInsensitive(t *testing.T) {
	for _, v := range []string{"glm", "GLM", "Glm"} {
		ps, err := NewPromptStrategy(v)
		if err != nil {
			t.Errorf("variant=%q 应成功: %v", v, err)
		}
		if ps.ID() != "glm" {
			t.Errorf("variant=%q 应命中 glm 策略，got ID=%q", v, ps.ID())
		}
	}
}

// ===== 用户画像注入 =====

// TestPromptStrategy_ProfileInjection 画像注入：文件存在拼 <user_profile> 块且置于消息最前，缺失/未配置路径不注入。
func TestPromptStrategy_ProfileInjection(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "profile.md")
	ctx := AskContext{SelectedText: "测试句子", Question: "这是什么"}

	ps, err := NewPromptStrategy("glm", WithProfilePath(profilePath))
	if err != nil {
		t.Fatalf("NewPromptStrategy: %v", err)
	}

	if got := ps.BuildUserMessage(ctx); strings.Contains(got, "<user_profile>") {
		t.Fatalf("画像缺失时不应注入: %q", got[:80])
	}

	if err := os.WriteFile(profilePath, []byte("用户是后端工程师"), 0o644); err != nil {
		t.Fatalf("写画像: %v", err)
	}
	got := ps.BuildUserMessage(ctx)
	if !strings.HasPrefix(got, "<user_profile>\n用户是后端工程师\n</user_profile>\n\n") {
		t.Fatalf("画像应注入在消息最前: %q", got[:80])
	}
	if !strings.Contains(got, "测试句子") {
		t.Fatalf("原始 user message 内容丢失: %q", got[:120])
	}

	ps2, err := NewPromptStrategy("glm")
	if err != nil {
		t.Fatalf("NewPromptStrategy: %v", err)
	}
	if got := ps2.BuildUserMessage(ctx); strings.Contains(got, "<user_profile>") {
		t.Fatalf("未配置画像路径时不应注入")
	}
}

// fakeMemReader 相关记忆检索假实现（Reader 最小实现，仅覆盖两个读方法）。
type fakeMemReader struct {
	entries     []*memory.Entry // SearchMemories 返回
	readEntries []*memory.Entry // ReadMemories 返回（缺省复用 entries）
	err         error
}

func (f *fakeMemReader) ReadMemories(ctx context.Context, userKey memory.UserKey, limit int) ([]*memory.Entry, error) {
	if f.readEntries == nil {
		return f.entries, f.err
	}
	return f.readEntries, f.err
}

func (f *fakeMemReader) SearchMemories(ctx context.Context, userKey memory.UserKey, query string, opts ...memory.SearchOption) ([]*memory.Entry, error) {
	return f.entries, f.err
}

// ===== relatedMemoriesBlock =====

// 检索有结果：注入 <related_memories> 块，位于画像之后、正文之前。
func TestRelatedMemoriesBlock_Assembly(t *testing.T) {
	s := &glmStrategy{memReader: &fakeMemReader{entries: []*memory.Entry{
		{Memory: &memory.Memory{Memory: "用户此前学过 GMP 调度与 work stealing"}},
	}}}
	ctx := AskContext{Question: "图的邻接表和哈希表有什么联系"}
	got := s.BuildUserMessage(ctx)

	if !strings.Contains(got, "<related_memories>") || !strings.Contains(got, "GMP 调度") {
		t.Fatalf("应注入相关记忆块: %q", got)
	}
	ip, ir := strings.Index(got, "<user_profile>"), strings.Index(got, "<related_memories>")
	ib := strings.Index(got, "图的邻接表")
	if !(ip < ir && ir < ib) {
		t.Fatalf("顺序应为 画像→相关记忆→正文: profile=%d related=%d body=%d", ip, ir, ib)
	}
}

// 无 reader（eval 工具链）/空问题：不注入。
func TestRelatedMemoriesBlock_NoReaderOrEmptyQuestion(t *testing.T) {
	s := &glmStrategy{}
	if got := s.BuildUserMessage(AskContext{Question: "这是什么"}); strings.Contains(got, "<related_memories>") {
		t.Fatalf("无 reader 不应注入")
	}
	s2 := &glmStrategy{memReader: &fakeMemReader{}}
	if got := s2.BuildUserMessage(AskContext{}); strings.Contains(got, "<related_memories>") {
		t.Fatalf("空问题不应注入")
	}
}

// 检索失败：回退 ReadMemories；两者都空/都失败：降级为空串，不阻断消息构建。
func TestRelatedMemoriesBlock_SearchError(t *testing.T) {
	s := &glmStrategy{memReader: &fakeMemReader{err: context.DeadlineExceeded}}
	got := s.BuildUserMessage(AskContext{Question: "什么意思"})
	if strings.Contains(got, "<related_memories>") {
		t.Fatalf("检索失败且回退失败应降级不注入: %q", got)
	}
	if !strings.Contains(got, "什么意思") {
		t.Fatalf("正文不应受影响")
	}
	s2 := &glmStrategy{memReader: &fakeMemReader{readEntries: []*memory.Entry{
		{Memory: &memory.Memory{Memory: "回退到的最近记忆"}},
	}}}
	got2 := s2.BuildUserMessage(AskContext{Question: "什么意思"})
	if !strings.Contains(got2, "<related_memories>") || !strings.Contains(got2, "回退到的最近记忆") {
		t.Fatalf("空结果应回退 ReadMemories 注入: %q", got2)
	}
	s3 := &glmStrategy{memReader: &fakeMemReader{}}
	if got3 := s3.BuildUserMessage(AskContext{Question: "什么意思"}); strings.Contains(got3, "<related_memories>") {
		t.Fatalf("无任何记忆时不应注入空壳块")
	}
}

// 超预算：只取头部条目，块内总字符不超过 relatedMemoryMaxRunes。
func TestRelatedMemoriesBlock_BudgetTruncation(t *testing.T) {
	long := strings.Repeat("忆", 1200)
	entries := make([]*memory.Entry, 0, 4)
	for i := 0; i < 4; i++ {
		entries = append(entries, &memory.Entry{Memory: &memory.Memory{Memory: long}})
	}
	s := &glmStrategy{memReader: &fakeMemReader{entries: entries}}
	got := s.relatedMemoriesBlock(AskContext{Question: "问"})
	n := len([]rune(got))
	if n > relatedMemoryMaxRunes+200 { // 头尾说明文本预留余量
		t.Errorf("块应按预算截断（got %d runes）", n)
	}
}

// 无划词 + 扩展端已采集页面内容：注入 <page_content>，不再提示 fetch_page。
func TestGLMStrategy_PageContentBlock_NoQuote(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{URL: "https://x.example.com/a", PageMd: "页面正文内容甲", Question: "这页讲什么？"}
	got := s.BuildUserMessage(ctx)
	if !strings.Contains(got, "<page_content>") || !strings.Contains(got, "页面正文内容甲") {
		t.Errorf("无划词 + PageMd 应注入 <page_content>。got: %q", got)
	}
	if strings.Contains(got, "fetch_page") || strings.Contains(got, "<page_url>") {
		t.Errorf("页面内容已注入，不应再出现 <page_url>/fetch_page。got: %q", got)
	}
}

// 有划词时不注入 <page_content>（reference_material 锚定来源页）。
func TestGLMStrategy_PageContentBlock_WithSelection(t *testing.T) {
	s := &glmStrategy{}
	ctx := AskContext{SelectedText: "术语", URL: "https://x.example.com/a", PageMd: "页面正文内容甲", Question: "是什么？"}
	got := s.BuildUserMessage(ctx)
	if strings.Contains(got, "<page_content>") {
		t.Errorf("有划词时不应注入 <page_content>。got: %q", got)
	}
	if !strings.Contains(got, "<reference_material>") {
		t.Errorf("划词材料应照常注入。got: %q", got)
	}
}

// PageMd 超预算：注入头部 6000 rune + 检索提示，不透传全文。
func TestPageContentBlock_HeadBudget(t *testing.T) {
	long := strings.Repeat("页", 7000)
	got := pageContentBlock(AskContext{URL: "https://x.example.com/a", PageMd: long})
	n := len([]rune(got))
	if n > 7000 {
		t.Errorf("<page_content> 应截断注入（got %d runes）", n)
	}
	if !strings.Contains(got, "expand_context") {
		t.Errorf("截断时应带 expand_context 检索提示")
	}
}

// ContentStatus 上报时渲染 <page_content_status> 状态块；未上报（空值）不渲染，提示词与历史行为一致。
func TestContentStatusBlock(t *testing.T) {
	s := &glmStrategy{}
	withStatus := AskContext{SelectedText: "划词内容", ContentStatus: "selected_only", Question: "是什么？"}
	got := s.BuildUserMessage(withStatus)
	if !strings.Contains(got, "<page_content_status>selected_only</page_content_status>") {
		t.Errorf("上报 contentStatus 应渲染状态块。got: %q", got)
	}
	if !strings.Contains(got, "不得假装已读过全文") {
		t.Errorf("状态块应带材料边界说明。got: %q", got)
	}
	noStatus := AskContext{SelectedText: "划词内容", Question: "是什么？"}
	got = s.BuildUserMessage(noStatus)
	if strings.Contains(got, "page_content_status") {
		t.Errorf("未上报 contentStatus 不应渲染状态块。got: %q", got)
	}
}
