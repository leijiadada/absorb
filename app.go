package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"trpc.group/trpc-go/trpc-agent-go/runner"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"absorb-agent/internal/bridge"
	"absorb-agent/internal/chat"
	"absorb-agent/internal/conf"
	"absorb-agent/internal/desktop"
	"absorb-agent/internal/extension"
	"absorb-agent/internal/log"
	"absorb-agent/internal/obs"
	"absorb-agent/internal/store"
	"absorb-agent/internal/store/storage"
	"absorb-agent/internal/upgrade"
	fetch "absorb-agent/tool/fetch_page"
)

// App 桌面 UI 绑定层：状态/设置/模型/目录打开/升级。
// 问答与取消不在此暴露——唯一链路是 HTTP 桥（扩展侧），桌面 UI 与流式解耦。
type App struct {
	ctx       context.Context
	cfgH      *conf.Handle // 配置运行时句柄：保存写入 / 请求侧读取（热切换）
	cfgPath   string
	dataRoot  string
	memGate   *chat.MemoryGate // 记忆运行时开关：SaveSettings 翻转即热切换
	saveMu    sync.Mutex       // SaveSettings 读-改-写串行化
	r         runner.Runner
	ps        chat.PromptStrategy
	sess      *store.SessionService
	reg       *chat.RunRegistry
	bridge    *bridge.Bridge
	upg       *upgrade.Service // 升级服务（配置关闭时为 nil；由 wiring 层装配）
	startedAt time.Time
}

// NewApp 组装桌面端绑定层。upg 为升级服务（nil = 升级功能关闭）。
func NewApp(cfgH *conf.Handle, cfgPath, dataRoot string, r runner.Runner, ps chat.PromptStrategy,
	sess *store.SessionService, reg *chat.RunRegistry, br *bridge.Bridge, memGate *chat.MemoryGate,
	upg *upgrade.Service) *App {
	return &App{
		cfgH: cfgH, cfgPath: cfgPath, dataRoot: dataRoot, memGate: memGate,
		r: r, ps: ps, sess: sess, reg: reg, bridge: br, upg: upg,
		startedAt: time.Now(),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go desktop.RunTray(ctx, appIconPNG)
	a.startUpdateCheck(ctx)
}

// startUpdateCheck 启动时后台检查更新（节流内跳过；发现新版经 Wails 事件通知前端，
// 不自动下载安装——必须用户确认）。
func (a *App) startUpdateCheck(ctx context.Context) {
	if a.upg == nil {
		return
	}
	go func() {
		if _, err := a.upg.CheckAndNotify(ctx); err != nil {
			log.Warnf("[Absorb] startup update check: %v", err)
		}
	}()
}

func (a *App) onSecondInstance(data options.SecondInstanceData) {
	runtime.WindowUnminimise(a.ctx)
	runtime.WindowShow(a.ctx)
}

func (a *App) shutdown(ctx context.Context) {
	a.bridge.Stop()
	obs.Close() // flush 剩余 span（幂等；未开启为 no-op）
}

// ---------- 状态仪表盘 ----------

// Status 状态页数据源（GetStatus 四卡 + 顶栏聚合点）。
type Status struct {
	ServiceRunning bool   `json:"serviceRunning"` // HTTP 桥启动成功且未失败
	Uptime         string `json:"uptime"`
	ExtConnected   bool   `json:"extConnected"` // 3 分钟内收到扩展心跳
	ExtLastSeen    int64  `json:"extLastSeen"`  // 最近心跳 Unix 秒，0=从未
	ExtState       string `json:"extState"`     // connected|disconnected|browser-closed|never
	ModelName      string `json:"modelName"`    // 空=未配置模型
	KeyConfigured  bool   `json:"keyConfigured"`
	ProxyURL       string `json:"proxyUrl"` // 空=未配置（可选，不阻断）
	MemoryEnabled  bool   `json:"memoryEnabled"`
	Version        string `json:"version"`
}

// GetStatus 桌面 UI 5s 轮询。模型卡取自默认模型配置（无默认=未配置）。
func (a *App) GetStatus() Status {
	startErr := a.bridge.StartErr()
	ls, conns, disc := a.bridge.ExtLink()
	state := extension.State(ls, conns, disc)
	modelName, keyConfigured := "", false
	if mc, err := a.sess.GetDefaultModelConfig(a.opCtx(), chat.AppName); err == nil && mc != nil {
		modelName = displayModelName(mc)
		keyConfigured = strings.TrimSpace(mc.APIKey) != ""
	}
	cur := a.cfgH.Get()
	return Status{
		ServiceRunning: startErr == nil,
		Uptime:         time.Since(a.startedAt).Round(time.Second).String(),
		ExtConnected:   state == "connected",
		ExtLastSeen:    ls,
		ExtState:       state,
		ModelName:      modelName,
		KeyConfigured:  keyConfigured,
		ProxyURL:       cur.Proxy.URL,
		MemoryEnabled:  cur.Memory.Enabled,
		Version:        versionDisplay(),
	}
}

// displayModelName 模型展示名：优先 DisplayName，否则模型 ID。
func displayModelName(mc *store.ModelConfig) string {
	if mc.DisplayName != "" {
		return mc.DisplayName
	}
	return mc.ModelID
}

// GetTokenStats token 用量统计（状态页"Token 用量"卡片，telemetry 第二批）：
// 今日/全期合计/按入口（chat/draft/summary）分布/今日缓存命中率。
// 统计是旁路信息：查询失败记日志并返回空结构（前端按零值展示），不阻塞页面。
func (a *App) GetTokenStats() storage.TokenUsageStatsResult {
	res, err := a.sess.TokenUsageStats(a.opCtx())
	if err != nil {
		log.Warnf("[Absorb] query token usage stats failed: %v", err)
		return storage.TokenUsageStatsResult{ByKind: map[string]storage.UsageTotals{}}
	}
	return res
}

// ---------- 设置 ----------

// Settings 设置页数据。
type Settings struct {
	MemoryEnabled bool   `json:"memoryEnabled"`
	ProxyURL      string `json:"proxyUrl"`
	SystemProxy   string `json:"systemProxy"`
	ProxyOverride string `json:"proxyOverride"`
	ConfigPath    string `json:"configPath"`
	DataRoot      string `json:"dataRoot"`
	Version       string `json:"version"`
}

// GetSettings 读取当前配置（运行时句柄快照）。系统代理为实时探测快照，供设置页展示"跟随系统"效果。
func (a *App) GetSettings() Settings {
	cur := a.cfgH.Get()
	s := Settings{
		MemoryEnabled: cur.Memory.Enabled,
		ProxyURL:      cur.Proxy.URL,
		ConfigPath:    a.cfgPath,
		DataRoot:      a.dataRoot,
		Version:       versionDisplay(),
	}
	if server, override, ok := fetch.SystemProxySnapshot(); ok {
		s.SystemProxy = server
		s.ProxyOverride = override
	}
	return s
}

// SaveSettings 落盘 config.yaml 并热切换运行时开关：代理地址由 fetch 工具与模型客户端
// 每次请求实时读取（句柄）；记忆开关翻转门控即时生效（服务常驻）。模型字段由模型页维护。
func (a *App) SaveSettings(s Settings) error {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	cur := a.cfgH.Get()
	next := cur
	next.Memory.Enabled = s.MemoryEnabled
	next.Proxy.URL = strings.TrimSpace(s.ProxyURL)
	memChanged := cur.Memory.Enabled != next.Memory.Enabled
	b, err := yaml.Marshal(&next)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(a.cfgPath, b, 0o600); err != nil {
		return err
	}
	a.cfgH.Set(next)
	if memChanged && a.memGate != nil {
		a.memGate.Set(next.Memory.Enabled)
	}
	return nil
}

// ---------- 升级 ----------

// UpdateView 前端升级状态视图（状态页/关于区展示）。
type UpdateView struct {
	Current   string `json:"current"`          // 当前版本（含构建时间）
	Latest    string `json:"latest,omitempty"` // feed 中的最新版本
	Available bool   `json:"available"`        // 有新版本可升级
	Mandatory bool   `json:"mandatory"`        // 低于最低要求版本，必须升级
	NotesURL  string `json:"notesUrl,omitempty"`
	Message   string `json:"message,omitempty"` // 人类可读状态说明
}

// GetUpdateStatus 最近一次检查结论（不触发网络；供前端展示与轮询）。
func (a *App) GetUpdateStatus() UpdateView {
	view := UpdateView{Current: versionDisplay()}
	if a.upg == nil {
		view.Message = "升级功能未启用"
		return view
	}
	res, err := a.upg.LastResult()
	if res.Current != "" {
		view.Current = res.Current
	}
	switch {
	case errors.Is(err, upgrade.ErrNoUpdate):
		view.Message = "已是最新版本"
	case err != nil:
		view.Message = err.Error()
	case res.Available:
		view.Available, view.Mandatory, view.Latest = true, res.Mandatory, res.Latest
		if res.Release != nil {
			view.NotesURL = res.Release.NotesURL
		}
	default:
		view.Message = "尚未检查更新"
	}
	return view
}

// CheckUpdate 手动检查更新（绕过节流；「检查更新」按钮调用）。
// 已是最新时返回 nil error，结论放在 Message。
func (a *App) CheckUpdate() (UpdateView, error) {
	if a.upg == nil {
		return UpdateView{Current: versionDisplay(), Message: "升级功能未启用"},
			upgrade.WithCode("not_enabled", errors.New("升级功能未启用"))
	}
	res, err := a.upg.Check(context.Background())
	view := UpdateView{
		Current:   res.Current,
		Latest:    res.Latest,
		Available: res.Available,
		Mandatory: res.Mandatory,
	}
	if res.Release != nil {
		view.NotesURL = res.Release.NotesURL
	}
	switch {
	case errors.Is(err, upgrade.ErrNoUpdate):
		view.Message = "已是最新版本"
	case err != nil:
		view.Message = err.Error()
		return view, err
	}
	return view, nil
}

// ApplyUpdate 下载并安装当前已发现的更新（需先 CheckUpdate 发现新版本）。
// 安装器拉起后自行结束旧进程并重启。
func (a *App) ApplyUpdate() error {
	if a.upg == nil {
		return upgrade.WithCode("not_enabled", errors.New("升级功能未启用"))
	}
	res, err := a.upg.LastResult()
	if err != nil || !res.Available || res.Release == nil {
		return upgrade.WithCode("no_pending", errors.New("没有可安装的更新，请先检查更新"))
	}
	return a.upg.Apply(a.opCtx(), *res.Release)
}

// ---------- 模型配置 ----------

// ModelConfigView 模型列表/编辑回显视图：Key 脱敏（只给 hasKey+尾4位），明文不下发。
type ModelConfigView struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	DisplayName  string `json:"displayName"`
	ModelID      string `json:"modelId"`
	BaseURL      string `json:"baseUrl"`
	FullURL      bool   `json:"fullUrl"`
	HasKey       bool   `json:"hasKey"`
	KeyMasked    string `json:"keyMasked"` // 如 …abcd；无 Key 为空
	MaxTokens    *int   `json:"maxTokens"`
	Thinking     int    `json:"thinking"`   // 0=跟随默认 1=开启 2=关闭
	ToolRounds   *int   `json:"toolRounds"` // nil=框架默认
	ImageSupport bool   `json:"imageSupport"`
	IsDefault    bool   `json:"isDefault"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

// ModelConfigInput 添加/编辑/测试共用的表单输入。编辑时 Key 留空=保留原值。
// 温度不在表单暴露（UI 对齐 Trae 精简），编辑时保留库中存量值。
type ModelConfigInput struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	DisplayName  string `json:"displayName"`
	ModelID      string `json:"modelId"`
	BaseURL      string `json:"baseUrl"`
	FullURL      bool   `json:"fullUrl"`
	APIKey       string `json:"apiKey"`
	MaxTokens    *int   `json:"maxTokens"`
	Thinking     int    `json:"thinking"`
	ToolRounds   *int   `json:"toolRounds"`
	ImageSupport bool   `json:"imageSupport"`
}

// ListModelConfigs 模型列表（含 isDefault 与脱敏 Key）。
func (a *App) ListModelConfigs() []ModelConfigView {
	rows, err := a.sess.ListModelConfigs(a.opCtx())
	if err != nil {
		log.Warnf("[Absorb] query model config failed: %v", err)
		return []ModelConfigView{}
	}
	defaultID, _ := a.sess.GetDefaultModelID(a.opCtx(), chat.AppName)
	views := make([]ModelConfigView, 0, len(rows))
	for i := range rows {
		views = append(views, a.modelConfigView(&rows[i], defaultID))
	}
	return views
}

// modelConfigView 存储行 → 前端视图（脱敏 + 默认标记）。
func (a *App) modelConfigView(mc *store.ModelConfig, defaultID string) ModelConfigView {
	return ModelConfigView{
		ID:           mc.ID,
		Provider:     mc.Provider,
		DisplayName:  mc.DisplayName,
		ModelID:      mc.ModelID,
		BaseURL:      mc.BaseURL,
		FullURL:      mc.FullURL,
		HasKey:       strings.TrimSpace(mc.APIKey) != "",
		KeyMasked:    maskKey(mc.APIKey),
		MaxTokens:    mc.MaxTokens,
		Thinking:     mc.Thinking,
		ToolRounds:   mc.ToolRounds,
		ImageSupport: mc.ImageSupport,
		IsDefault:    mc.ID == defaultID,
		CreatedAt:    mc.CreatedAt.Format("2006-01-02 15:04"),
		UpdatedAt:    mc.UpdatedAt.Format("2006-01-02 15:04"),
	}
}

// maskKey Key 掩码：只露尾 4 位。
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) <= 4 {
		return "****"
	}
	return "…" + string(r[len(r)-4:])
}

// validate 校验表单输入并规整字段。
func (in ModelConfigInput) validate() (*store.ModelConfig, error) {
	modelID := strings.TrimSpace(in.ModelID)
	if modelID == "" {
		return nil, fmt.Errorf("请填写模型 ID")
	}
	base := strings.TrimSpace(in.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("请填写请求地址")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("请求地址需以 http:// 或 https:// 开头")
	}
	thinking := in.Thinking
	if thinking < 0 || thinking > 2 {
		thinking = 0
	}
	if in.ToolRounds != nil && *in.ToolRounds < 1 {
		return nil, fmt.Errorf("工具调用轮数需为正整数，留空使用默认")
	}
	return &store.ModelConfig{
		Provider:     strings.TrimSpace(in.Provider),
		DisplayName:  strings.TrimSpace(in.DisplayName),
		ModelID:      modelID,
		APIFormat:    "openai_chat",
		BaseURL:      base,
		FullURL:      in.FullURL,
		APIKey:       strings.TrimSpace(in.APIKey),
		MaxTokens:    in.MaxTokens,
		Thinking:     thinking,
		ToolRounds:   in.ToolRounds,
		ImageSupport: in.ImageSupport,
	}, nil
}

// AddModelConfig 新增模型配置；模型表为空时自动设为默认。
func (a *App) AddModelConfig(in ModelConfigInput) error {
	mc, err := in.validate()
	if err != nil {
		return err
	}
	existing, err := a.sess.ListModelConfigs(a.opCtx())
	if err != nil {
		return err
	}
	mc.ID = store.NewModelID()
	if err := a.sess.CreateModelConfig(a.opCtx(), mc); err != nil {
		return err
	}
	if len(existing) == 0 {
		return a.sess.SetDefaultModelID(a.opCtx(), chat.AppName, mc.ID)
	}
	return nil
}

// UpdateModelConfig 编辑模型配置；Key 留空=保留原值。
func (a *App) UpdateModelConfig(in ModelConfigInput) error {
	if in.ID == "" {
		return fmt.Errorf("缺少模型配置 ID")
	}
	old, err := a.sess.GetModelConfig(a.opCtx(), in.ID)
	if err != nil {
		return err
	}
	if old == nil {
		return fmt.Errorf("该模型配置不存在")
	}
	mc, err := in.validate()
	if err != nil {
		return err
	}
	if mc.APIKey == "" {
		mc.APIKey = old.APIKey // 编辑留空 = 保留原 Key
	}
	mc.Temperature = old.Temperature // 温度不在表单暴露，编辑保留原值
	mc.ID = old.ID
	mc.CreatedAt = old.CreatedAt
	return a.sess.UpdateModelConfig(a.opCtx(), mc)
}

// DeleteModelConfig 删除模型配置；删的是默认时把默认转移到 updated_at 最近的一条，
// 列表清空则回到"未配置"态。
func (a *App) DeleteModelConfig(id string) error {
	ctx := a.opCtx()
	defaultID, err := a.sess.GetDefaultModelID(ctx, chat.AppName)
	if err != nil {
		return err
	}
	if err := a.sess.DeleteModelConfig(ctx, id); err != nil {
		return err
	}
	if defaultID != id {
		return nil
	}
	rest, err := a.sess.ListModelConfigs(ctx)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return a.sess.ClearDefaultModelID(ctx, chat.AppName)
	}
	latest := rest[0]
	for _, mc := range rest[1:] {
		if mc.UpdatedAt.After(latest.UpdatedAt) {
			latest = mc
		}
	}
	return a.sess.SetDefaultModelID(ctx, chat.AppName, latest.ID)
}

// SetDefaultModel 切换默认模型（问答即时生效，无需重启）。
func (a *App) SetDefaultModel(id string) error {
	mc, err := a.sess.GetModelConfig(a.opCtx(), id)
	if err != nil {
		return err
	}
	if mc == nil {
		return fmt.Errorf("该模型配置不存在")
	}
	return a.sess.SetDefaultModelID(a.opCtx(), chat.AppName, id)
}

// TestModelConnection 用表单当前值发一次真实请求（消耗少量 Token），返回延迟或人话错误。
// 编辑态 Key 留空时用原 Key 测试。
func (a *App) TestModelConnection(in ModelConfigInput) chat.TestResult {
	mc, err := in.validate()
	if err != nil {
		return chat.TestResult{Error: err.Error()}
	}
	if mc.APIKey == "" && in.ID != "" {
		if old, err := a.sess.GetModelConfig(a.opCtx(), in.ID); err == nil && old != nil {
			mc.APIKey = old.APIKey
		}
	}
	return chat.TestModelConn(mc, func() string { return a.cfgH.Get().Proxy.URL })
}

// ---------- 记忆画像 / 目录 ----------

// GetProfile 读取 L3 画像文件（不存在返回空串）。
func (a *App) GetProfile() string {
	b, err := os.ReadFile(a.cfgH.Get().Memory.Profile.FilePath)
	if err != nil {
		return ""
	}
	return string(b)
}

// opCtx 绑定方法共用的操作上下文：本地 SQLite 查询毫秒级完成，无需超时控制。
func (a *App) opCtx() context.Context {
	return context.Background()
}

// ---------- 扩展管理（转发 internal/extension） ----------

// ExportExtension 释放内嵌扩展到数据目录并定位文件夹（GUI 模式弹资源管理器）。
func (a *App) ExportExtension() extension.ExportResult {
	return extension.Export(a.dataRoot, a.ctx != nil)
}

// OpenExtensionsPage 打开本机浏览器的扩展管理页。
func (a *App) OpenExtensionsPage() extension.ExportResult {
	return extension.OpenExtensionsPage()
}
