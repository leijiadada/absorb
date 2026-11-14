package chat

// 集成式单测：用真实 runner（NewRunner）+ httptest 假 openai 端点，
// 验证 AskHandler 对多模态请求的处理：
//  1. 视觉模型（ImageSupport=true）：发往模型的请求体含 image_url，且不被前置闸门拦截。
//  2. GLM（ImageSupport=true，不迁移/不改元数据）：图片照样发出（触发端点 1210），
//     且 1210 作为 SSE type:error 回显给用户（即“触发 1210 提示错误就行”）。
//  3. 非视觉模型（ImageSupport=false）：前置闸门直接拦截，根本不调用模型。
//
// 注：图片实际被序列化为 image_url 的细节由 TestMultimodalSerialization_Spike 在
// openai 客户端层验证；本测试在 handler→runner 全链路层面确认图片确实被转发。
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/knowledge"
	"absorb-agent/internal/store"

	"trpc.group/trpc-go/trpc-agent-go/session"
)

// capture 线程安全地记录假端点收到的请求体。
type capture struct {
	mu   sync.Mutex
	body []byte
}

func (c *capture) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.body)
}

// 默认成功流：一段正常 token + [DONE]，finish_reason 为 null（无异常）。
const defaultOKStream = "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
	"data: [DONE]\n\n"

// 异常流：先下发部分 token，最终 chunk 用 finish_reason 携带异常原因（HTTP 仍 200）。
const (
	contentFilterStream = "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n" +
		"data: [DONE]\n\n"
	lengthStream = "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
)

// setupOpts setup 可选项集合（wsFn / ks 注入）。
type setupOpts struct {
	wsFn func() conf.WorkingSetConfig
	ks   *knowledge.Service
	ksOn bool
}

// setupOpt setup 的单项配置。
type setupOpt func(*setupOpts)

// withKnowledge 启用 M3.1 关联检索：setup 内部用会话库同一存储构建知识服务
// （测试再经返回的 svc.Storage() 造数据，请求期才真正检索）。
func withKnowledge() setupOpt { return func(o *setupOpts) { o.ksOn = true } }

// withWSFn 覆盖工作集配置读取函数（不传 = 默认启用零值配置）。
func withWSFn(fn func() conf.WorkingSetConfig) setupOpt { return func(o *setupOpts) { o.wsFn = fn } }

// setup 构建一个指向假端点的完整 runner + handler，并把指定 provider/ImageSupport 的模型设为默认。
// failWith1210=true 时假端点返回 400 + 1210 错误（模拟 GLM 拒收图片）；
// okStream 返回 SSE 文本流，供需要指定 finish_reason 的用例使用；
// 其余可选项经 setupOpt 注入（withWSFn / withKS）。
func setup(t *testing.T, provider string, imageSupport, failWith1210 bool, okStream string, opts ...setupOpt) (http.HandlerFunc, *capture, *store.SessionService) {
	t.Helper()
	c := &capture{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.body = b
		c.mu.Unlock()
		if failWith1210 {
			w.WriteHeader(http.StatusBadRequest)
			// 复刻真实 GLM 端点返回体（扁平 code/message，非 {"error":{}} 包裹）。
			_, _ = io.WriteString(w, `{"code":"1210","message":"messages.content.type 参数非法，取值范围 ['text']"}`)
			return
		}
		// 验证“请求体已带图片/已调用模型”，响应是否能被框架完整解析不影响本测试断言。
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, okStream)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	svc, err := store.NewSessionService(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() }) // 释放 SQLite 文件锁，避免 t.TempDir 清理失败被判 FAIL
	ctx := context.Background()
	mc := &store.ModelConfig{
		ID:           "m1",
		Provider:     provider,
		ModelID:      "test-model",
		BaseURL:      srv.URL,
		FullURL:      true,
		APIKey:       "test-key",
		ImageSupport: imageSupport,
	}
	if err := svc.CreateModelConfig(ctx, mc); err != nil {
		t.Fatalf("CreateModelConfig: %v", err)
	}
	if err := svc.UpdateAppState(ctx, AppName, session.StateMap{"default_model_id": []byte("m1")}); err != nil {
		t.Fatalf("UpdateAppState: %v", err)
	}

	cfg := conf.Config{}
	cfg.Memory.DBPath = ":memory:" // 记忆库用内存模式，避免测试结束时文件锁导致 t.TempDir 清理失败
	cfg.Fetch.TimeoutSeconds = 30
	ps, err := NewPromptStrategy("")
	if err != nil {
		t.Fatalf("NewPromptStrategy: %v", err)
	}
	r, err := NewRunner(cfg, func() string { return "" }, ps, svc, NewMemoryGate(false))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	reg := NewRunRegistry()
	opt := setupOpts{wsFn: func() conf.WorkingSetConfig { return cfg.WorkingSet }} // 零值 = 默认启用（现有用例会话为空，L1 自然空注入）
	for _, fn := range opts {
		fn(&opt)
	}
	if opt.ksOn {
		opt.ks = knowledge.NewService(svc.Storage())
	}
	h := AskHandler(r, ps, svc, AppName, "absorb-user", reg, opt.wsFn, opt.ks)
	return h, c, svc
}

// askWithImage 发起一次带单张 png 图片的 /api/ask 请求，返回 SSE 输出全文。
func askWithImage(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"question": "描述这张图",
		"images":   []map[string]any{{"dataUrl": pngDataURL, "mime": "image/png"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

func TestAskHandler_Multimodal(t *testing.T) {
	t.Run("vision_model_forwards_image_url", func(t *testing.T) {
		h, c, _ := setup(t, "openai", true, false, defaultOKStream)
		out := askWithImage(t, h)
		reqBody := c.get()
		if !strings.Contains(reqBody, "image_url") {
			t.Fatalf("视觉模型请求体未含 image_url: %s", reqBody)
		}
		if !strings.Contains(reqBody, pngDataURL) {
			t.Fatalf("视觉模型请求体未携带图片 data URL")
		}
		if strings.Contains(out, `"type":"error"`) {
			t.Fatalf("视觉模型不应报错，实际 SSE: %s", out)
		}
	})

	// bigmodel(GLM) 不被前置闸门拦截，图片照样发出 → 触发端点 1210。
	// 流式 SDK 在 4xx 路径只把状态文本放进错误并丢弃 body，故 1210 到不了 errs；
	// 精确的 1210 → model.image_unsupported 映射由 errs 包的 TestFromModel_BigModel1210InHTTPText 锁定。
	t.Run("bigmodel_forwards_image_and_surfaces_error", func(t *testing.T) {
		h, c, _ := setup(t, "bigmodel", true, true, defaultOKStream)
		out := askWithImage(t, h)
		reqBody := c.get()
		if !strings.Contains(reqBody, "image_url") {
			t.Fatalf("bigmodel 应发出图片以触发 1210，但请求体无 image_url: %s", reqBody)
		}
		if !strings.Contains(out, `"type":"error"`) {
			t.Fatalf("bigmodel 1210 应作为 type:error 回显，实际 SSE: %s", out)
		}
	})

	t.Run("non_vision_model_blocked_by_gate", func(t *testing.T) {
		h, c, _ := setup(t, "openai", false, false, defaultOKStream)
		out := askWithImage(t, h)
		reqBody := c.get()
		// 前置闸门直接拦截，模型根本不应被调用
		if len(reqBody) != 0 {
			t.Fatalf("非视觉模型不应调用模型，但请求体非空: %s", reqBody)
		}
		if !strings.Contains(out, `"type":"error"`) {
			t.Fatalf("应返回“不支持图片”错误，实际 SSE: %s", out)
		}
	})
}

// askText 发起一次纯文本（无图片）的 /api/ask 请求，返回 SSE 输出全文。
func askText(t *testing.T, h http.HandlerFunc, q string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"question": q})
	req := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

// TestAskHandler_FinishReason 验证：生成中途异常时，厂商不在 HTTP 错误体返回，
// 而是把原因写在 200 流的 finish_reason 里；handler 必须据此下发 type:error
// 且归并到正确稳定码，且不发 done。这里用 DeepSeek provider（OpenAI 兼容格式）复现。
func TestAskHandler_FinishReason_ContentFilter(t *testing.T) {
	h, _, _ := setup(t, "deepseek", true, false, contentFilterStream)
	out := askText(t, h, "敏感问题")
	if !strings.Contains(out, `"type":"error"`) {
		t.Fatalf("content_filter 应作为 type:error 回显，实际 SSE: %s", out)
	}
	if !strings.Contains(out, "model.content_policy") {
		t.Fatalf("content_filter 应归并到 model.content_policy，实际 SSE: %s", out)
	}
	if strings.Contains(out, `"done":true`) {
		t.Fatalf("异常 finish_reason 不应下发 done，实际 SSE: %s", out)
	}
}

func TestAskHandler_FinishReason_Length(t *testing.T) {
	h, _, _ := setup(t, "deepseek", true, false, lengthStream)
	out := askText(t, h, "长问题")
	if !strings.Contains(out, `"type":"error"`) {
		t.Fatalf("length 应作为 type:error 回显，实际 SSE: %s", out)
	}
	if !strings.Contains(out, "model.context_too_long") {
		t.Fatalf("length 应归并到 model.context_too_long，实际 SSE: %s", out)
	}
}
