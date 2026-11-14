package chat

// 验证 trpc-agent-go 的 openai 后端对多模态（文本+图片）请求的序列化行为。
// 用 httptest 假端点捕获实际发出的请求 body，不依赖 API Key 与网络。
//
// 关闭 text-only 时 content 序列化为数组并含 type=image_url；
// 开启 text-only 时图片 part 被丢弃，content 只剩纯文本。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
)

const pngDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func TestMultimodalSerialization_Spike(t *testing.T) {
	const text = "描述这张图"

	cases := []struct {
		name      string
		textOnly  bool
		wantImage bool
	}{
		{"text_only_drops_image", true, false},
		{"multimodal_keeps_image", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{
					"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"glm-4v",
					"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
				}`)
			}))
			defer srv.Close()

			// 直接构造 openai 客户端（与生产 buildOpenAIModel 同一底层 API）。
			// glm variant 默认 textOnlyMessageContent=false，这里显式用选项覆盖以确定性验证。
			m := openai.New("glm-4v",
				openai.WithBaseURL(srv.URL),
				openai.WithAPIKey("test"),
				openai.WithVariant(openai.Variant("glm")),
				openai.WithTextOnlyMessageContent(tc.textOnly),
			)

			msg := model.NewUserMessage(text)
			msg.AddImageURL(pngDataURL, "auto") // 框架辅助方法：追加一个图片 part

			ch, err := m.GenerateContent(context.Background(), &model.Request{
				Messages:         []model.Message{msg},
				GenerationConfig: model.GenerationConfig{Stream: false},
			})
			if err != nil {
				t.Fatalf("GenerateContent 返回错误: %v", err)
			}
			for range ch { // drain，避免 goroutine 泄漏
			}

			var body struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(captured, &body); err != nil {
				t.Fatalf("解析请求 body 失败: %v\nbody=%s", err, string(captured))
			}
			var userMsg *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			for i := range body.Messages {
				if body.Messages[i].Role == "user" {
					userMsg = &body.Messages[i]
				}
			}
			if userMsg == nil {
				t.Fatalf("请求中未找到 user 消息\nbody=%s", string(captured))
			}
			content := string(userMsg.Content)
			hasImage := strings.Contains(content, "image_url") && strings.Contains(content, pngDataURL)

			if tc.wantImage {
				if !hasImage {
					t.Fatalf("期望 content 含 image_url，实际: %s", content)
				}
				if !strings.Contains(content, text) {
					t.Fatalf("期望 content 仍含文本，实际: %s", content)
				}
				if !strings.HasPrefix(strings.TrimSpace(content), "[") {
					t.Fatalf("期望 content 为数组，实际: %s", content)
				}
			} else {
				if hasImage {
					t.Fatalf("期望图片被丢弃，但 content 仍含 image_url: %s", content)
				}
			}
			t.Logf("[%s] content=%s", tc.name, content)
		})
	}
}
