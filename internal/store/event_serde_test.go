package store

import (
	"encoding/json"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// 验证 event.Event 的 JSON 序列化/反序列化往返一致——
// SQLite 会话存储依赖此往返正确性（AppendEvent 写入、GetSession 读出）。
func TestEventJSONRoundTrip(t *testing.T) {
	finish := "stop"
	orig := &event.Event{
		Response: &model.Response{
			ID:     "resp-123",
			Object: "chat.completion",
			Model:  "glm-4-flash",
			Choices: []model.Choice{{
				Index: 0,
				Message: model.Message{
					Role:    "assistant",
					Content: "区块链是一种分布式账本技术，数据不可篡改。",
				},
				FinishReason: &finish,
			}},
		},
		RequestID:    "req-abc",
		InvocationID: "inv-1",
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var got event.Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// 验证关键字段
	if got.Response.Choices[0].Message.Content != "区块链是一种分布式账本技术，数据不可篡改。" {
		t.Errorf("Message.Content = %q, want %q",
			got.Response.Choices[0].Message.Content,
			orig.Response.Choices[0].Message.Content)
	}
}
