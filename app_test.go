package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/bridge"
	"absorb-agent/internal/chat"
	"absorb-agent/internal/conf"
	"absorb-agent/internal/extension"
	"absorb-agent/internal/store"
)

// newTestApp 构造带临时 SQLite 会话库的 App（不启动窗口/桥/托盘）。
func newTestApp(t *testing.T) *App {
	t.Helper()
	sess, err := store.NewSessionService(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("创建临时会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	cfg := &conf.Config{}
	cfg.Model.Name = "glm-4.6"
	cfg.Log.Dir = "logs"

	return &App{
		cfgH:      conf.NewHandle(*cfg),
		cfgPath:   filepath.Join(t.TempDir(), "config.yaml"),
		dataRoot:  t.TempDir(),
		memGate:   chat.NewMemoryGate(cfg.Memory.Enabled),
		sess:      sess,
		bridge:    bridge.New(7788, "test", bridge.Deps{}),
		startedAt: time.Now(),
	}
}

func TestGetStatusServiceRunning(t *testing.T) {
	app := newTestApp(t)
	st := app.GetStatus()
	if !st.ServiceRunning {
		t.Fatalf("正常桥应 ServiceRunning=true, got %+v", st)
	}
	// 模型卡读自模型配置表：未配置时为空
	if st.Version != desktopVersion || st.ModelName != "" || st.KeyConfigured {
		t.Fatalf("版本/模型名不符: %+v", st)
	}
}

func TestGetStatusExtStates(t *testing.T) {
	app := newTestApp(t)
	// 浏览器进程探测用 stub 固定返回值
	stubRunning := false
	extension.RunningProbe = func() bool { return stubRunning }
	t.Cleanup(func() { extension.RunningProbe = extension.BrowserRunning })

	if st := app.GetStatus(); st.ExtConnected || st.ExtState != "never" {
		t.Fatal("从未活跃应 never")
	}

	// Native 管道连接 → connected（优先于 HTTP 心跳）
	app.bridge.SetNativeConns(1)
	if st := app.GetStatus(); !st.ExtConnected || st.ExtState != "connected" {
		t.Fatalf("Native 管道在应 connected, got %s", st.ExtState)
	}
	// 管道断开（计数归零）→ 浏览器进程在 → disconnected（秒级感知，不依赖心跳窗口）
	app.bridge.SetNativeConns(0)
	app.bridge.SetDisconnectAt(time.Now().Unix())
	stubRunning = true
	if st := app.GetStatus(); st.ExtConnected || st.ExtState != "disconnected" {
		t.Fatalf("Native 断开+浏览器在应 disconnected, got %s", st.ExtState)
	}
	// 断开 + 浏览器进程不在 → browser-closed
	stubRunning = false
	if st := app.GetStatus(); st.ExtState != "browser-closed" {
		t.Fatalf("Native 断开+浏览器不在应 browser-closed, got %s", st.ExtState)
	}

	// 重连后回到 connected，断开痕迹被清除
	app.bridge.NativeConnect(100)
	if st := app.GetStatus(); !st.ExtConnected || st.ExtState != "connected" {
		t.Fatalf("Native 重连应 connected, got %s", st.ExtState)
	}
	app.bridge.NativeDisconnect(100)

	// 全部管道断开 + 浏览器不在 → browser-closed
	if st := app.GetStatus(); st.ExtConnected || st.ExtState != "browser-closed" {
		t.Fatalf("管道全断+浏览器不在应 browser-closed, got %s", st.ExtState)
	}
}

func TestSaveSettingsKeepsModelFieldsUntouched(t *testing.T) {
	app := newTestApp(t)
	app.cfgH.Set(conf.Config{Memory: conf.MemoryConfig{Enabled: false}, Proxy: conf.ProxyConfig{URL: "http://127.0.0.1:7897"}})

	// 设置页已不含模型字段：保存只影响记忆/代理
	if err := app.SaveSettings(Settings{MemoryEnabled: true, ProxyURL: "http://127.0.0.1:7890"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	cur := app.cfgH.Get()
	if !cur.Memory.Enabled {
		t.Fatalf("内存配置未更新: memory=%v", cur.Memory.Enabled)
	}
	// 热切换：门控应同步翻转
	if !app.memGate.Enabled() {
		t.Fatalf("记忆门控未随保存翻转")
	}
	// 落盘验证：重新加载 yaml
	back := conf.LoadConfig(app.cfgPath)
	if !back.Memory.Enabled {
		t.Fatalf("落盘配置不符: memory=%v", back.Memory.Enabled)
	}
}

// ---------- 模型配置 ----------

func TestModelConfigBindings(t *testing.T) {
	app := newTestApp(t)

	// 未配置模型：状态页无默认
	if st := app.GetStatus(); st.ModelName != "" || st.KeyConfigured {
		t.Fatalf("未配置应 modelName=空 key=false, got %+v", st)
	}

	// 校验：缺模型 ID
	if err := app.AddModelConfig(ModelConfigInput{Provider: "bigmodel", BaseURL: "https://x"}); err == nil || !strings.Contains(err.Error(), "模型 ID") {
		t.Fatalf("缺模型 ID 应报错, err=%v", err)
	}
	// 校验：地址非 http(s)
	if err := app.AddModelConfig(ModelConfigInput{ModelID: "m", BaseURL: "ftp://x"}); err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("非法地址应报错, err=%v", err)
	}

	// 首条自动设为默认
	if err := app.AddModelConfig(ModelConfigInput{
		Provider: "bigmodel", ModelID: "glm-4.6", BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKey: "sk-abcd1234",
	}); err != nil {
		t.Fatalf("AddModelConfig: %v", err)
	}
	views := app.ListModelConfigs()
	if len(views) != 1 || !views[0].IsDefault {
		t.Fatalf("首条应为默认: %+v", views)
	}
	// Key 脱敏：hasKey + 尾 4 位，明文不下发
	if !views[0].HasKey || views[0].KeyMasked != "…1234" || strings.Contains(views[0].KeyMasked, "sk-") {
		t.Fatalf("Key 脱敏不符: hasKey=%v masked=%q", views[0].HasKey, views[0].KeyMasked)
	}
	if st := app.GetStatus(); st.ModelName != "glm-4.6" || !st.KeyConfigured {
		t.Fatalf("状态页应跟随默认模型: %+v", st)
	}

	// 追加第二条，默认不变
	if err := app.AddModelConfig(ModelConfigInput{
		Provider: "deepseek", ModelID: "deepseek-chat", BaseURL: "https://api.deepseek.com", APIKey: "sk-dskl8888",
	}); err != nil {
		t.Fatalf("AddModelConfig 2: %v", err)
	}
	views = app.ListModelConfigs()
	if len(views) != 2 || !views[0].IsDefault || views[1].IsDefault {
		t.Fatalf("默认不应随追加变化: %+v", views)
	}

	// 切换默认：状态页即时跟随
	if err := app.SetDefaultModel(views[1].ID); err != nil {
		t.Fatalf("SetDefaultModel: %v", err)
	}
	if st := app.GetStatus(); st.ModelName != "deepseek-chat" {
		t.Fatalf("切默认后状态页模型不符: %+v", st)
	}
}

// TestModelConfigAdvancedFields 高级配置字段：三态思考、工具轮数、图片输入。
func TestModelConfigAdvancedFields(t *testing.T) {
	app := newTestApp(t)

	// 工具轮数非正整数拦截
	if err := app.AddModelConfig(ModelConfigInput{
		ModelID: "m", BaseURL: "https://x", ToolRounds: ptrInt(0),
	}); err == nil || !strings.Contains(err.Error(), "正整数") {
		t.Fatalf("轮数 0 应报错, err=%v", err)
	}
	// 思考越界值规整为跟随默认
	if err := app.AddModelConfig(ModelConfigInput{
		Provider: "bigmodel", ModelID: "glm-4.6", BaseURL: "https://x", APIKey: "k",
		Thinking: 7, ToolRounds: ptrInt(8), ImageSupport: false,
	}); err != nil {
		t.Fatalf("AddModelConfig: %v", err)
	}
	mc, _ := app.sess.GetModelConfig(app.opCtx(), app.ListModelConfigs()[0].ID)
	if mc.Thinking != 0 {
		t.Fatalf("越界思考值应规整为 0, got %d", mc.Thinking)
	}
	if mc.ToolRounds == nil || *mc.ToolRounds != 8 || mc.ImageSupport {
		t.Fatalf("新字段未落库: rounds=%v img=%v", mc.ToolRounds, mc.ImageSupport)
	}

	// 三态读写往返：1=开 2=关 0=跟随
	for _, want := range []int{1, 2, 0} {
		modelID := fmt.Sprintf("tri-%d", want)
		if err := app.AddModelConfig(ModelConfigInput{
			Provider: "deepseek", ModelID: modelID, BaseURL: "https://x", APIKey: "k", Thinking: want,
		}); err != nil {
			t.Fatalf("AddModelConfig %s: %v", modelID, err)
		}
		var rowID string
		for _, v := range app.ListModelConfigs() {
			if v.ModelID == modelID {
				rowID = v.ID
			}
		}
		got, err := app.sess.GetModelConfig(app.opCtx(), rowID)
		if err != nil || got == nil {
			t.Fatalf("GetModelConfig %s: %v %v", rowID, got, err)
		}
		if got.Thinking != want {
			t.Fatalf("三态往返 %d != %d", got.Thinking, want)
		}
	}
}

func ptrInt(v int) *int { return &v }

func TestModelConfigUpdateKeepsEmptyKey(t *testing.T) {
	app := newTestApp(t)
	if err := app.AddModelConfig(ModelConfigInput{
		Provider: "bigmodel", ModelID: "glm-4.6", BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKey: "original-key-9999",
	}); err != nil {
		t.Fatalf("AddModelConfig: %v", err)
	}
	id := app.ListModelConfigs()[0].ID

	// Key 留空 = 保留原值
	if err := app.UpdateModelConfig(ModelConfigInput{
		ID: id, Provider: "bigmodel", ModelID: "glm-4.5-air", BaseURL: "https://open.bigmodel.cn/api/paas/v4",
	}); err != nil {
		t.Fatalf("UpdateModelConfig: %v", err)
	}
	mc, _ := app.sess.GetModelConfig(app.opCtx(), id)
	if mc.ModelID != "glm-4.5-air" || mc.APIKey != "original-key-9999" {
		t.Fatalf("更新结果不符: model=%s key=%q", mc.ModelID, mc.APIKey)
	}

	// Key 非空 = 覆盖
	if err := app.UpdateModelConfig(ModelConfigInput{
		ID: id, Provider: "bigmodel", ModelID: "glm-4.5-air", BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKey: "new-key-1111",
	}); err != nil {
		t.Fatalf("UpdateModelConfig 2: %v", err)
	}
	if mc, _ := app.sess.GetModelConfig(app.opCtx(), id); mc.APIKey != "new-key-1111" {
		t.Fatalf("新 Key 未覆盖: %q", mc.APIKey)
	}
}

func TestModelConfigDeleteDefaultTransfers(t *testing.T) {
	app := newTestApp(t)
	if err := app.AddModelConfig(ModelConfigInput{
		Provider: "bigmodel", ModelID: "glm-4.6", BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKey: "k1",
	}); err != nil {
		t.Fatalf("AddModelConfig: %v", err)
	}
	if err := app.AddModelConfig(ModelConfigInput{
		Provider: "deepseek", ModelID: "deepseek-chat", BaseURL: "https://api.deepseek.com", APIKey: "k2",
	}); err != nil {
		t.Fatalf("AddModelConfig 2: %v", err)
	}
	views := app.ListModelConfigs()
	defaultView := views[0] // glm-4.6 为默认
	otherID := views[1].ID

	// bump other 的 updated_at（秒级精度，间隔 1.1s 保证可比较）
	time.Sleep(1100 * time.Millisecond)
	if err := app.UpdateModelConfig(ModelConfigInput{
		ID: otherID, Provider: "deepseek", ModelID: "deepseek-chat", BaseURL: "https://api.deepseek.com", APIKey: "k2",
	}); err != nil {
		t.Fatalf("UpdateModelConfig: %v", err)
	}

	// 删默认 → 转移到 updated_at 最近的一条
	if err := app.DeleteModelConfig(defaultView.ID); err != nil {
		t.Fatalf("DeleteModelConfig: %v", err)
	}
	rest := app.ListModelConfigs()
	if len(rest) != 1 || !rest[0].IsDefault || rest[0].ModelID != "deepseek-chat" {
		t.Fatalf("默认应转移到剩余最近一条: %+v", rest)
	}

	// 删最后一条 → 回到"未配置"态
	if err := app.DeleteModelConfig(rest[0].ID); err != nil {
		t.Fatalf("DeleteModelConfig 2: %v", err)
	}
	if left := app.ListModelConfigs(); len(left) != 0 {
		t.Fatalf("应清空: %+v", left)
	}
	if st := app.GetStatus(); st.ModelName != "" || st.KeyConfigured {
		t.Fatalf("清空后应回到未配置: %+v", st)
	}
}

func TestMigrateLegacyModelConfig(t *testing.T) {
	svc, err := store.NewSessionService(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("创建临时会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()

	// config.yaml 无模型信息：不迁移
	ok, err := svc.MigrateLegacyModelConfig(ctx, chat.AppName, "", "", "", 0, 0, false)
	if ok || err != nil {
		t.Fatalf("空配置不应迁移: ok=%v err=%v", ok, err)
	}

	// 完整配置：导入为第一条默认，provider 按 base_url 推断
	ok, err = svc.MigrateLegacyModelConfig(ctx, chat.AppName, "glm-4.6", "sk-yaml-key-7777",
		"https://open.bigmodel.cn/api/paas/v4", 0.7, 0, true)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !ok {
		t.Fatal("应执行迁移")
	}
	mc, err := svc.GetDefaultModelConfig(ctx, chat.AppName)
	if err != nil || mc == nil {
		t.Fatalf("迁移后应有默认模型: %v %v", mc, err)
	}
	if mc.ModelID != "glm-4.6" || mc.Provider != "bigmodel" || mc.APIKey != "sk-yaml-key-7777" {
		t.Fatalf("迁移字段不符: %+v", mc)
	}
	if mc.Temperature == nil || *mc.Temperature != 0.7 || mc.Thinking != 1 || mc.MaxTokens != nil {
		t.Fatalf("迁移参数不符: temp=%v thinking=%v max=%v", mc.Temperature, mc.Thinking, mc.MaxTokens)
	}

	// 二次启动：表已有数据，不重复迁移
	ok, err = svc.MigrateLegacyModelConfig(ctx, chat.AppName, "another", "key", "https://x", 0, 0, false)
	if ok || err != nil {
		t.Fatalf("已有数据不应重复迁移: ok=%v err=%v", ok, err)
	}
	if list, _ := svc.ListModelConfigs(ctx); len(list) != 1 {
		t.Fatalf("迁移不应产生重复行: %d", len(list))
	}
}

// openaiStub 起一个 OpenAI 兼容桩服务（/chat/completions）。
func openaiStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelConnectionStub(t *testing.T) {
	app := newTestApp(t)

	// 正常响应 → ok
	okBody, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-x", "object": "chat.completion", "created": 1, "model": "test-model",
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop"}},
	})
	srv := openaiStub(t, http.StatusOK, string(okBody))
	res := app.TestModelConnection(ModelConfigInput{
		Provider: "custom", ModelID: "test-model", BaseURL: srv.URL, APIKey: "sk-stub",
	})
	if !res.OK || res.Error != "" {
		t.Fatalf("桩服务应连通成功: %+v", res)
	}

	// 401 → errs 归一化为人话错误（稳定码 + 中性英文兜底文案，前端按码 i18n）
	srv401 := openaiStub(t, http.StatusUnauthorized,
		`{"error":{"message":"Invalid API key provided","type":"invalid_request_error"}}`)
	res = app.TestModelConnection(ModelConfigInput{
		Provider: "custom", ModelID: "test-model", BaseURL: srv401.URL, APIKey: "bad",
	})
	if res.OK || !strings.Contains(res.Error, "API key invalid") {
		t.Fatalf("401 应转密钥错误文案: %+v", res)
	}

	// 编辑态 Key 留空 → 用原 Key 测试（桩校验 Authorization 头）
	var gotAuth string
	srvAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(string(okBody)))
	}))
	t.Cleanup(srvAuth.Close)
	if err := app.AddModelConfig(ModelConfigInput{
		Provider: "custom", ModelID: "test-model", BaseURL: srvAuth.URL, APIKey: "sk-original",
	}); err != nil {
		t.Fatalf("AddModelConfig: %v", err)
	}
	id := app.ListModelConfigs()[0].ID
	res = app.TestModelConnection(ModelConfigInput{
		ID: id, Provider: "custom", ModelID: "test-model", BaseURL: srvAuth.URL,
	})
	if !res.OK {
		t.Fatalf("留空 Key 测试应成功: %+v", res)
	}
	if gotAuth != "Bearer sk-original" {
		t.Fatalf("留空 Key 应使用原 Key: %q", gotAuth)
	}
}
