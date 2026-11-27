// Package read_page 网页章节读取工具：在缓存的页面结构化文本（pageMd，
// markdown 标题已转换）上按章节定位与阅读。
//
// expand_context 的关键词子串匹配靠模型猜词、无相关性排序，命中质量不稳定；
// read_page 按标题结构确定性切分——outline 列目录、按序号读整节，模型不猜词、
// 定位准确率 100%。页级阅读（已入库文档）走 search_document 的 start_page
// 模式，两者职责互不重叠。
package readpage

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// 单次章节输出预算（与 <page_content> 注入、search_document 读页同量级）。
const sectionBudgetRunes = 6000

// pageSectionState 单次请求内的章节索引状态。
type pageSectionState struct {
	pageText string
}

type pageSectionKey struct{}

// WithPageSectionState 注入页面章节索引状态；pageText 为空时工具返回
// 无缓存失败信号。
func WithPageSectionState(ctx context.Context, pageText string) context.Context {
	return context.WithValue(ctx, pageSectionKey{}, &pageSectionState{pageText: pageText})
}

func stateFromContext(ctx context.Context) *pageSectionState {
	if v, ok := ctx.Value(pageSectionKey{}).(*pageSectionState); ok {
		return v
	}
	return nil
}

// section 一个章节：标题层级 + 标题 + 行区间（含标题行）。
type section struct {
	index int    // 1-based 序号（目录展示用）
	level int    // 标题级别 1-6
	title string // 标题文本（无标题前导区为"导语"）
	lines []string
}

// splitSections 按 markdown 标题行切分章节；首个标题前的非空内容归入
// "导语"节（无前导内容则不产出）。
func splitSections(text string) []section {
	var out []section
	cur := section{title: "导语"}
	started := false
	flush := func() {
		if strings.TrimSpace(strings.Join(cur.lines, "\n")) != "" {
			cur.index = len(out) + 1
			out = append(out, cur)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		level := headingLevel(line)
		if level > 0 {
			flush()
			started = true
			cur = section{
				level: level,
				title: strings.TrimSpace(strings.TrimLeft(line, "#")),
				lines: []string{line},
			}
			continue
		}
		if !started && strings.TrimSpace(line) == "" {
			continue // 导语前的空行不产出空节
		}
		cur.lines = append(cur.lines, line)
	}
	flush()
	return out
}

// headingLevel 标题行级别：^#{1,6}+ 空格开头返回 1-6，否则 0。
func headingLevel(line string) int {
	n := 0
	for _, r := range line {
		if r == '#' {
			n++
			continue
		}
		if r == ' ' || r == '\t' {
			break
		}
		return 0
	}
	if n >= 1 && n <= 6 {
		return n
	}
	return 0
}

func (s *section) text() string {
	return strings.TrimSpace(strings.Join(s.lines, "\n"))
}

func (s *section) runes() int {
	return len([]rune(s.text()))
}

type readPageInput struct {
	// 章节序号（来自 outline 目录的序号）；0 或缺省 = 返回章节目录。
	Section int `json:"section,omitempty"`
}

type readPageOutput struct {
	Success bool   `json:"success"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`
}

// NewReadPageTool 注册 read_page 工具。
func NewReadPageTool() tool.CallableTool {
	fn := func(ctx context.Context, in readPageInput) (readPageOutput, error) {
		st := stateFromContext(ctx)
		if st == nil || strings.TrimSpace(st.pageText) == "" {
			return readPageOutput{Success: false, Message: "本会话没有缓存的页面内容（划词时未获取到页面），无法按章节阅读。请基于已有材料回答。"}, nil
		}
		_, span := otel.Tracer("absorb/readpage").Start(ctx, "read_page")
		defer span.End()
		sections := splitSections(st.pageText)
		if len(sections) == 0 {
			return readPageOutput{Success: false, Message: "页面内容为空，无法按章节阅读。"}, nil
		}
		if in.Section <= 0 {
			var b strings.Builder
			b.WriteString("页面章节目录（传 section 序号读取对应章节全文）：\n")
			for _, s := range sections {
				prefix := ""
				if s.level > 0 {
					prefix = strings.Repeat("#", s.level) + " "
				}
				fmt.Fprintf(&b, "%d. %s%s（约 %d 字）\n", s.index, prefix, s.title, s.runes())
			}
			span.SetAttributes(attribute.Int("sections", len(sections)))
			return readPageOutput{Success: true, Content: b.String()}, nil
		}
		if in.Section > len(sections) {
			return readPageOutput{Success: false, Message: fmt.Sprintf("章节序号越界：本页共 %d 个章节，请先查看目录。", len(sections))}, nil
		}
		s := sections[in.Section-1]
		txt := s.text()
		runes := []rune(txt)
		if len(runes) > sectionBudgetRunes {
			txt = string(runes[:sectionBudgetRunes]) + "…（本节超长已截断）"
		}
		span.SetAttributes(
			attribute.Int("section", in.Section),
			attribute.Int("section_runes", len([]rune(txt))),
		)
		return readPageOutput{
			Success: true,
			Content: fmt.Sprintf("【第 %d 节】%s\n\n%s", s.index, s.title, txt),
		}, nil
	}
	return function.NewFunctionTool(fn,
		function.WithName("read_page"),
		function.WithDescription(
			"按章节阅读当前网页的完整原文。两种用法：" +
				"1) 不传参数：返回页面章节目录（序号、标题、字数）。" +
				"2) 传 section 序号：读取该章节的完整原文（标题来自目录）。" +
				"当问题需要整篇文章的论证结构、某个章节的完整内容，或划选材料只是局部而问题涉及全文时调用；" +
				"比关键词检索更可靠——按目录定位，不依赖猜测原文用词。" +
				"不适用：解释划选内容本身（直接回答）；获取其他网页（用 fetch_page）；" +
				"在已入库文档（PDF 等）中查找（用 search_document）。" +
				"调用时不要输出正文说明，直接发起调用。",
		),
		function.WithInputSchema(&tool.Schema{
			Type:     "object",
			Required: []string{},
			Properties: map[string]*tool.Schema{
				"section": {
					Type:        "integer",
					Description: "章节序号（1-based，来自目录）；缺省返回章节目录",
				},
			},
		}),
	)
}
