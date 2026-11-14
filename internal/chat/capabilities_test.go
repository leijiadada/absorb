package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/store"
)

// newCapabilitiesSvc 构建临时 SessionService；imageSupport 非 nil 时创建并设为默认模型配置。
func newCapabilitiesSvc(t *testing.T, imageSupport *bool) *store.SessionService {
	t.Helper()
	svc, err := store.NewSessionService(filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() }) // 释放 SQLite 文件锁，避免 t.TempDir 清理失败被判 FAIL
	if imageSupport == nil {
		return svc
	}
	ctx := context.Background()
	if err := svc.CreateModelConfig(ctx, &store.ModelConfig{
		ID: "m1", Provider: "bigmodel", ModelID: "test-model",
		APIKey: "test-key", ImageSupport: *imageSupport,
	}); err != nil {
		t.Fatalf("CreateModelConfig: %v", err)
	}
	if err := svc.UpdateAppState(ctx, AppName, session.StateMap{"default_model_id": []byte("m1")}); err != nil {
		t.Fatalf("UpdateAppState: %v", err)
	}
	return svc
}

// TestCapabilitiesHandler GET /api/capabilities 按默认模型下发 imageSupport，
// 覆盖三态：视觉模型 true / 非视觉模型 false / 无模型配置宽容默认 true
// （与 AskHandler 前置闸门 mc==nil 放行语义一致，见 capabilities.go）。
func TestCapabilitiesHandler(t *testing.T) {
	t.Parallel()
	tcase := []struct {
		name string
		caps *bool // nil = 不配置模型
		want bool
	}{
		{"vision_model", boolPtr(true), true},
		{"text_only_model", boolPtr(false), false},
		{"no_model_default_open", nil, true},
	}
	for _, tc := range tcase {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newCapabilitiesSvc(t, tc.caps)
			h := CapabilitiesHandler(svc, AppName)
			req := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var got struct {
				ImageSupport bool `json:"imageSupport"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.ImageSupport != tc.want {
				t.Fatalf("imageSupport = %v, want %v", got.ImageSupport, tc.want)
			}
			// CORS 响应头：GET 简单请求无预检，扩展端跨域读取依赖此头（同 /health）
			if rec.Header().Get("Access-Control-Allow-Origin") == "" {
				t.Fatal("missing Access-Control-Allow-Origin header")
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
