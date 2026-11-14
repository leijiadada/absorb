package expand

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/tool"

	"absorb-agent/tool/pageretriever"
)

// mustExtract 读本包 testdata fixture 并抽取结构化文本（TestRunExpand_AttemptLadder 用）
func mustExtract(t *testing.T, fixture string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out := retriever.ExtractPageText(string(b))
	if out == "" {
		t.Fatal("ExtractPageText returned empty")
	}
	return out
}

func TestExpandContextInputKeywordsDecode(t *testing.T) {
	// LLM tool_call arguments 的 JSON 必须能解析出 keywords（唯一字段）
	raw := `{"keywords":["Runner","多Agent Runner"]}`
	var in expandContextInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(in.Keywords) != 2 || in.Keywords[0] != "Runner" {
		t.Errorf("Keywords = %v", in.Keywords)
	}

	// 兼容旧格式：reason/hint 已移出 schema，模型残留输出应被忽略不报错
	rawLegacy := `{"reason":"需要定义","hint":"查找定义","keywords":["Runner"]}`
	var inLegacy expandContextInput
	if err := json.Unmarshal([]byte(rawLegacy), &inLegacy); err != nil {
		t.Fatalf("旧格式解析失败: %v", err)
	}
	if len(inLegacy.Keywords) != 1 || inLegacy.Keywords[0] != "Runner" {
		t.Errorf("Keywords = %v", inLegacy.Keywords)
	}
}

func TestRunExpand_SingleCall(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")
	refMat := "调度器会在每个 tick 重新计算优先级，保证高优任务先执行。"

	// 无缓存：失败消息
	out := RunExpand("", "ref", []string{"Runner"})
	if out.Success || out.Message == "" {
		t.Errorf("no cache should fail with message, got %+v", out)
	}

	// 带 keywords：定向检索，收集全部命中
	out = RunExpand(pt, refMat, []string{"Runner"})
	if !out.Success {
		t.Fatalf("should hit, got %+v", out)
	}
	if !strings.Contains(out.Content, "命中标题") {
		t.Errorf("should have heading hit, got %+v", out)
	}
	if !strings.Contains(out.Content, "---") {
		t.Errorf("multiple hits should be joined with separator, got %+v", out)
	}

	// 无 keywords：参考材料邻近窗口
	out = RunExpand(pt, refMat, nil)
	if !out.Success || !strings.Contains(out.Content, "调度器") {
		t.Errorf("no-keywords should fallback to ref window, got %+v", out)
	}

	// 零命中关键词：返回零命中信号
	out = RunExpand(pt, refMat, []string{"LangGraph"})
	if !out.Success || !strings.Contains(out.Content, "未出现") {
		t.Errorf("zero-hit should return signal, got %+v", out)
	}
}

// ---------- L2 工具接线测试：走真实注册→调用路径（WithPageSearchState 注入 ctx → NewExpandContextTool().Call） ----------

// callExpandTool 构造 tool 调用参数并驱动真实工具
func callExpandTool(t *testing.T, ctl tool.CallableTool, ctx context.Context, kw []string) expandContextOutput {
	t.Helper()
	raw, err := json.Marshal(expandContextInput{Keywords: kw})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := ctl.Call(ctx, raw)
	if err != nil {
		t.Fatalf("tool call error: %v", err)
	}
	out, ok := res.(expandContextOutput)
	if !ok {
		t.Fatalf("result type = %T, want expandContextOutput", res)
	}
	return out
}

// 有状态：ctx 注入 pageText+refMat → 按 keywords 定向检索多命中合并返回
func TestExpandTool_CallWithState(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")
	refMat := "调度器会在每个 tick 重新计算优先级，保证高优任务先执行。"
	ctl := NewExpandContextTool()
	ctx := WithPageSearchState(context.Background(), pt, refMat)

	out := callExpandTool(t, ctl, ctx, []string{"Runner"})
	if !out.Success {
		t.Fatalf("should hit, got %+v", out)
	}
	if !strings.Contains(out.Content, "命中标题") || !strings.Contains(out.Content, "Runner 是 tRPC-Agent-Go") {
		t.Errorf("content should carry hit sections, got: %+v", out)
	}
	if !strings.Contains(out.Content, "---") {
		t.Errorf("multiple hits should be joined with separator, got: %+v", out)
	}
}

// 无状态：ctx 未注入 → 工具返回"检索状态不可用"
func TestExpandTool_CallNoState(t *testing.T) {
	ctl := NewExpandContextTool()
	out := callExpandTool(t, ctl, context.Background(), []string{"Runner"})
	if out.Success {
		t.Fatalf("no-state should fail, got %+v", out)
	}
	if !strings.Contains(out.Message, "检索状态不可用") {
		t.Errorf("no-state message wrong: %q", out.Message)
	}
}

// 无缓存：注入空 pageText 应返回失败。
func TestExpandTool_CallNoCache(t *testing.T) {
	ctl := NewExpandContextTool()
	ctx := WithPageSearchState(context.Background(), "", "x")
	out := callExpandTool(t, ctl, ctx, []string{"Runner"})
	if out.Success {
		t.Fatalf("no-cache should fail, got %+v", out)
	}
	if !strings.Contains(out.Message, "本会话没有缓存的页面内容") {
		t.Errorf("no-cache message wrong: %q", out.Message)
	}
}
