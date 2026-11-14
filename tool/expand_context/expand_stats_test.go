package expand

import (
	"context"
	"encoding/json"
	"testing"
)

// TestRunExpandStats 四条路径的观测数据：命中 / 零命中 / 无关键词回退 / 无缓存。
func TestRunExpandStats(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")
	refMat := "调度器会在每个 tick 重新计算优先级，保证高优任务先执行。"

	// 命中：ZeroHit=false、结果与缓存非空、未走回退
	out, st := runExpand(pt, refMat, []string{"Runner"})
	if !out.Success || st.ZeroHit || st.ResultRunes == 0 || st.CacheRunes == 0 || st.Fallback != "" {
		t.Fatalf("hit stats wrong: out=%+v stats=%+v", out, st)
	}

	// 零命中：ZeroHit=true、走的是 keywords 路径（无回退标记）
	out, st = runExpand(pt, refMat, []string{"LangGraph"})
	if !out.Success || !st.ZeroHit || st.Fallback != "" {
		t.Fatalf("zero-hit stats wrong: out=%+v stats=%+v", out, st)
	}

	// 无关键词：回退 ref_mat_window，不计零命中
	out, st = runExpand(pt, refMat, nil)
	if !out.Success || st.Fallback != "ref_mat_window" || st.ZeroHit || st.ResultRunes == 0 {
		t.Fatalf("fallback stats wrong: out=%+v stats=%+v", out, st)
	}

	// 无缓存：CacheRunes=0、失败消息
	out, st = runExpand("", refMat, []string{"Runner"})
	if out.Success || st.CacheRunes != 0 || st.ZeroHit || st.ResultRunes != 0 {
		t.Fatalf("no-cache stats wrong: out=%+v stats=%+v", out, st)
	}
}

// TestExpandToolCallCounter 同一请求状态多次调用，call# 应递增——
// 零命中后"是否二次检索"的观测依赖这个计数。
func TestExpandToolCallCounter(t *testing.T) {
	ctl := NewExpandContextTool()
	ctx := WithPageSearchState(context.Background(), "页面包含 Runner 一词", "参考材料")
	raw, err := json.Marshal(expandContextInput{Keywords: []string{"Runner"}})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := ctl.Call(ctx, raw); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	st := pageSearchStateFromContext(ctx)
	if st == nil {
		t.Fatal("state should be present")
	}
	if n := st.calls.Load(); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
}
