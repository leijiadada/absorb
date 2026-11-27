package chat

// buildWorkingSetL1（工作集 L1 组装）表驱动测试：
// 分支去重（跨 scope 保留 / 同 scope 与无标轮次丢弃 / 空 filterKey 全丢弃）、
// 工具轮折叠标记、超预算降级（从最旧轮剔除）、估算非零。

import (
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"

	"absorb-agent/internal/kind"
	"absorb-agent/internal/store"
	"absorb-agent/internal/store/storage"
)

// wsRound 构造一个测试轮次（user + 最终 assistant，同 FilterKey）。
// Version 必须置 CurrentVersion：evt.Filter 仅在 Version 匹配时读 FilterKey，
// 否则回退 Branch（空 → 恒匹配）；生产事件由 runner 经 event.New 携带当前版本。
func wsRound(userContent, assistantContent, filterKey string, toolCount int) storage.RecentRound {
	return storage.RecentRound{
		UserEvent: event.Event{Version: event.CurrentVersion, FilterKey: filterKey, Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{Role: model.RoleUser, Content: userContent}}},
		}},
		FinalEvent: event.Event{Version: event.CurrentVersion, FilterKey: filterKey, Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{Role: model.RoleAssistant, Content: assistantContent}}},
		}},
		ToolCount: toolCount,
	}
}

func TestBuildWorkingSetL1(t *testing.T) {
	t.Run("跨scope保留同scope去重", func(t *testing.T) {
		rounds := []storage.RecentRound{
			wsRound("A1问", "A1答", "https://a.com", 0),
			wsRound("B问", "B答", "https://b.com", 0),
			wsRound("A2问", "A2答", "https://a.com", 0),
		}
		msgs, stats := buildWorkingSetL1(rounds, "https://b.com", 16000, false)
		if stats.Selected != 3 || stats.Kept != 2 || stats.DroppedDup != 1 {
			t.Errorf("stats = %+v, want selected=3 kept=2 dropped=1", stats)
		}
		if len(msgs) != 4 {
			t.Fatalf("msgs = %d, want 4", len(msgs))
		}
		// 升序：A1(user+assistant) → A2(user+assistant)；B 轮（当前 scope）被去重
		if msgs[0].Role != model.RoleUser || msgs[0].Content != "A1问" ||
			msgs[1].Role != model.RoleAssistant || msgs[1].Content != "A1答" ||
			msgs[2].Content != "A2问" || msgs[3].Content != "A2答" {
			t.Errorf("消息序列错误: %+v", msgs)
		}
		for _, m := range msgs {
			if strings.Contains(m.Content, "B问") || strings.Contains(m.Content, "B答") {
				t.Errorf("当前 scope 轮次应被去重，仍出现: %q", m.Content)
			}
		}
	})

	t.Run("空filterKey全丢弃", func(t *testing.T) {
		rounds := []storage.RecentRound{wsRound("问", "答", "https://a.com", 0)}
		msgs, stats := buildWorkingSetL1(rounds, "", 16000, false)
		if msgs != nil {
			t.Fatalf("空 filterKey 应全丢弃: msgs=%v", msgs)
		}
		if stats.Kept != 0 || stats.DroppedDup != 1 {
			t.Errorf("stats = %+v", stats)
		}
	})

	t.Run("无标轮次在非空filterKey下同样丢弃", func(t *testing.T) {
		// 无标（FilterKey=""）事件会被 SDK 纳入任意 scope 请求（Prefix 过滤），注入即双花
		rounds := []storage.RecentRound{wsRound("旧问", "旧答", "", 0)}
		msgs, stats := buildWorkingSetL1(rounds, "https://b.com", 16000, false)
		if msgs != nil {
			t.Fatalf("无标轮次应丢弃: msgs=%v", msgs)
		}
		if stats.DroppedDup != 1 {
			t.Errorf("stats = %+v", stats)
		}
	})

	t.Run("general轮双向语义", func(t *testing.T) {
		// 生产形态：general 轮事件 FK="general"（非哈希、互不为前缀），文章轮 FK=哈希。
		rounds := []storage.RecentRound{
			wsRound("什么是RAG", "RAG答", kind.ScopeGeneral, 0),
			wsRound("文章B问", "文章B答", store.ScopeID("https://b.com"), 0),
		}
		fkB := store.ScopeID("https://b.com")

		// 文章请求：general 轮 Filter=false → L1 保留注入（跨文章连续性）；文章轮去重
		msgs, stats := buildWorkingSetL1(rounds, fkB, 16000, false)
		if stats.Kept != 1 || stats.DroppedDup != 1 {
			t.Errorf("文章请求 stats = %+v, want kept=1 dropped=1", stats)
		}
		if len(msgs) != 2 || msgs[0].Content != "什么是RAG" || msgs[1].Content != "RAG答" {
			t.Errorf("general 轮应注入文章请求: %+v", msgs)
		}

		// general 请求：general 轮 Filter=true 去重（SDK 已原样重放）；文章轮经 L1 注入
		msgs, stats = buildWorkingSetL1(rounds, kind.ScopeGeneral, 16000, false)
		if stats.Kept != 1 || stats.DroppedDup != 1 {
			t.Errorf("general 请求 stats = %+v, want kept=1 dropped=1", stats)
		}
		if len(msgs) != 2 || msgs[0].Content != "文章B问" || msgs[1].Content != "文章B答" {
			t.Errorf("文章轮应注入 general 请求: %+v", msgs)
		}
	})

	t.Run("工具轮折叠标记", func(t *testing.T) {
		rounds := []storage.RecentRound{wsRound("问", "最终答", "https://a.com", 3)}
		msgs, _ := buildWorkingSetL1(rounds, "https://b.com", 16000, false)
		want := "（该轮含 3 次工具调用，详情可回溯）\n\n最终答"
		if msgs[1].Content != want {
			t.Errorf("assistant 内容 = %q, want %q", msgs[1].Content, want)
		}
		if msgs[1].ReasoningContent != "" {
			t.Errorf("不应回放 ReasoningContent")
		}
		if len(msgs[1].ToolCalls) != 0 {
			t.Errorf("不应回放 ToolCalls")
		}
	})

	t.Run("超预算降级剔除最旧轮", func(t *testing.T) {
		rounds := []storage.RecentRound{
			wsRound("旧问", "旧答", "https://a.com", 0),
			wsRound("中问", "中答", "https://a.com", 0),
			wsRound("新问", "新答", "https://a.com", 0),
		}
		// 每轮估算成本 = 2 rune × 2 条消息 = 4 tokens（(2*2+2)/3=2 每条）。
		// 预算 5 只够 1 轮：剔除最旧 2 轮，保留最近 1 轮
		msgs, stats := buildWorkingSetL1(rounds, "https://b.com", 5, false)
		if stats.TrimmedOld != 2 {
			t.Fatalf("TrimmedOld = %d, want 2（剔除最旧 2 轮）", stats.TrimmedOld)
		}
		if stats.Kept != 3 || stats.Selected != 3 {
			t.Errorf("stats = %+v（降级不影响 kept 口径）", stats)
		}
		if len(msgs) != 2 || msgs[0].Content != "新问" || msgs[1].Content != "新答" {
			t.Errorf("应仅保留最近一轮: %+v", msgs)
		}
		if stats.EstTokens != 4 {
			t.Errorf("EstTokens = %d, want 4（实际注入量）", stats.EstTokens)
		}
	})

	t.Run("单轮即超预算全部剔除", func(t *testing.T) {
		rounds := []storage.RecentRound{wsRound("问", "答", "https://a.com", 0)}
		msgs, stats := buildWorkingSetL1(rounds, "https://b.com", 1, false)
		if len(msgs) != 0 {
			t.Errorf("单轮即超预算应剔除为空: %+v", msgs)
		}
		if stats.TrimmedOld != 1 || stats.Messages != 0 || stats.EstTokens != 0 {
			t.Errorf("stats = %+v, want trimmed=1 msgs=0 est=0", stats)
		}
	})

	t.Run("预算内不降级", func(t *testing.T) {
		rounds := []storage.RecentRound{
			wsRound("问一", "答一", "https://a.com", 0),
			wsRound("问二", "答二", "https://a.com", 0),
		}
		msgs, stats := buildWorkingSetL1(rounds, "https://b.com", 16000, false)
		if stats.TrimmedOld != 0 {
			t.Errorf("预算内不应剔除: %+v", stats)
		}
		if len(msgs) != 4 || msgs[0].Content != "问一" || msgs[3].Content != "答二" {
			t.Errorf("预算内注入序列不变: %+v", msgs)
		}
	})

	t.Run("估算非零", func(t *testing.T) {
		if estimateTokens("一二三四五六") <= 0 {
			t.Errorf("estimateTokens 应 > 0")
		}
	})

	t.Run("缩略图门控keepImages", func(t *testing.T) {
		// user 消息带 1 个 image part（库内形态=落库缩略图 dataURL）+ 文本提问
		mkRound := func() storage.RecentRound {
			rd := wsRound("带图问", "带图答", "https://a.com", 0)
			rd.UserEvent.Response.Choices[0].Message.ContentParts = []model.ContentPart{
				{Type: model.ContentType("image"), Image: &model.Image{URL: "data:image/jpeg;base64,AAAA"}},
			}
			return rd
		}
		rounds := []storage.RecentRound{mkRound()}

		// keepImages=false（非视觉默认模型，读取失败保守关闭同此）：过滤 image part，
		// 不注入图片存在性标记
		msgs, stats := buildWorkingSetL1(rounds, "https://b.com", 16000, false)
		if len(msgs) != 2 {
			t.Fatalf("msgs = %d, want 2", len(msgs))
		}
		if len(msgs[0].ContentParts) != 0 {
			t.Errorf("非视觉模型应过滤 image part: %+v", msgs[0].ContentParts)
		}
		if strings.Contains(msgs[0].Content, "图片") {
			t.Errorf("非视觉模型不应有图片标记: %q", msgs[0].Content)
		}
		if msgs[0].Content != "带图问" {
			t.Errorf("Content = %q, want 带图问", msgs[0].Content)
		}
		_ = stats

		// keepImages=true（视觉默认模型）：保留 part + 文本前缀图片存在性标记
		// （标记随文本自然流进 L2/L3 摘要，与 Trae 压缩后「图片仅剩文字提及」同构）
		msgs, _ = buildWorkingSetL1(rounds, "https://b.com", 16000, true)
		if len(msgs[0].ContentParts) != 1 || msgs[0].ContentParts[0].Type != model.ContentType("image") {
			t.Fatalf("视觉模型应保留 image part: %+v", msgs[0].ContentParts)
		}
		if !strings.HasPrefix(msgs[0].Content, "[本轮含 1 张图片（缩略图，已降采样）]\n\n") {
			t.Errorf("应注入图片存在性标记前缀: %q", msgs[0].Content)
		}
		if !strings.Contains(msgs[0].Content, "带图问") {
			t.Errorf("原文应保留: %q", msgs[0].Content)
		}
	})
}
