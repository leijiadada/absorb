package chat

// M3.1 关联提示集成测试：/api/ask 全链路（真实 runner + 假模型端点）验证
// associations SSE 事件的触发条件与载荷形状：
//  1. summary 轮次：无划词、库中有匹配知识 → done 前下发 associations，kind=summary；
//  2. selection 轮次：带划词上下文 → kind=selection，检索词取划选文字；
//  3. 零命中 / 未启用：静默（无 associations 事件），done 正常下发——
//     "没有可靠关联就正常完成当前任务"，不强行制造联系。
//
// 检索为纯规则关键词 + LIKE 召回（M3.1 已过评测门槛），此处断言的是接线行为
// 而非召回质量（质量回归见 internal/knowledge 的 TestRetrievalEvalQuality）。
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"absorb-agent/internal/knowledge"
	"absorb-agent/internal/store/storage"
)

// askPlain 发起一次无图片的 /api/ask 请求，返回 SSE 输出全文。
func askPlain(t *testing.T, h http.HandlerFunc, body map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

// seedKnowledge 直接经存储层造一条 active 知识（绕过 Service.Create 的
// origin 会话校验——本测试关注检索接线，不关注保存链路）。
func seedKnowledge(t *testing.T, svc storage.Storage, id, title, quote string) {
	t.Helper()
	it := &storage.KnowledgeItem{
		ID:              id,
		AppName:         AppName,
		UserID:          UserID,
		Title:           title,
		AISummary:       title,
		Status:          storage.KnowledgeStatusActive,
		OriginSessionID: "s-assoc",
		OriginEventID:   "e-assoc",
		DedupKey:        "dk-" + id,
	}
	if _, _, err := svc.CreateKnowledge(context.Background(), it.AppName, it.UserID, it, []*storage.KnowledgeSource{{
		ID: "ks-" + id, ItemID: it.ID, AppName: it.AppName, UserID: it.UserID,
		SourceType: storage.KnowledgeSourceQuote, QuoteText: quote,
	}}); err != nil {
		t.Fatalf("seed knowledge: %v", err)
	}
}

// extractAssociations 从 SSE 全文取 associations 事件（无则返回 nil）。
func extractAssociations(t *testing.T, sse string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &evt); err != nil {
			continue
		}
		if evt["type"] == "associations" {
			return evt
		}
	}
	return nil
}

func TestAskAssociations_SummaryHit(t *testing.T) {
	h, _, svc := setup(t, "openai", true, false, defaultOKStream, withKnowledge())
	seedKnowledge(t, svc.Storage(), "k1", "检索质量决定 RAG 回答质量", "RAG 回答不佳时应先检查检索结果")

	out := askPlain(t, h, map[string]any{"question": "什么是检索质量？"})
	evt := extractAssociations(t, out)
	if evt == nil {
		t.Fatalf("库中有匹配知识，应下发 associations 事件。SSE: %s", out)
	}
	if evt["kind"] != string(knowledge.RetrievalKindSummary) {
		t.Errorf("无划词轮次 kind 应为 summary，实际 %v", evt["kind"])
	}
	if evt["requestId"] == "" {
		t.Errorf("associations 应携带 requestId 供前端对位")
	}
	cands, _ := evt["candidates"].([]any)
	if len(cands) == 0 {
		t.Fatalf("associations.candidates 不应为空: %v", evt)
	}
	first, _ := cands[0].(map[string]any)
	if first["itemId"] != "k1" {
		t.Errorf("应命中造入的知识条目，实际 itemId=%v", first["itemId"])
	}
	// done 仍正常下发且在 associations 之后（前端流内同步消费的前提）
	if i := strings.Index(out, `"type":"associations"`); i > strings.Index(out, `"done":true`) {
		t.Errorf("associations 应先于 done 下发")
	}
}

func TestAskAssociations_SelectionKind(t *testing.T) {
	h, _, svc := setup(t, "openai", true, false, defaultOKStream, withKnowledge())
	seedKnowledge(t, svc.Storage(), "k2", "向量混合召回与 Reranker 的分工", "召回负责找得全，精排负责排得好")

	out := askPlain(t, h, map[string]any{
		"question": "为什么有了向量检索还需要重排？",
		"context":  map[string]any{"text": "BM25 与向量混合召回", "url": "https://example.com/a"},
	})
	evt := extractAssociations(t, out)
	if evt == nil {
		t.Fatalf("划词轮次应下发 associations 事件。SSE: %s", out)
	}
	if evt["kind"] != string(knowledge.RetrievalKindSelection) {
		t.Errorf("带划词轮次 kind 应为 selection，实际 %v", evt["kind"])
	}
}

func TestAskAssociations_SilentWhenNoMatch(t *testing.T) {
	t.Run("enabled_but_empty_knowledge", func(t *testing.T) {
		h, _, _ := setup(t, "openai", true, false, defaultOKStream, withKnowledge())
		out := askPlain(t, h, map[string]any{"question": "光合作用的原理是什么？"})
		if extractAssociations(t, out) != nil {
			t.Errorf("无相关知识时不应下发 associations（低干扰原则）。SSE: %s", out)
		}
		if !strings.Contains(out, `"done":true`) {
			t.Errorf("done 应正常下发。SSE: %s", out)
		}
	})
	t.Run("retrieval_disabled", func(t *testing.T) {
		h, _, svc := setup(t, "openai", true, false, defaultOKStream) // 未 withKnowledge
		seedKnowledge(t, svc.Storage(), "k1", "检索质量决定 RAG 回答质量", "RAG 回答不佳时应先检查检索结果")
		out := askPlain(t, h, map[string]any{"question": "什么是检索质量？"})
		if extractAssociations(t, out) != nil {
			t.Errorf("未启用检索（ks=nil）时不应下发 associations。SSE: %s", out)
		}
	})
}
