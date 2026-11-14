package conf

import (
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// DefaultUpgradeFeedURL 默认升级 feed 地址（仓库内 releases/latest.json 的 raw 地址）。
// 发布流程（.github/workflows/release.yml）在打 tag 后更新该文件，客户端据此发现新版本。
const DefaultUpgradeFeedURL = "https://raw.githubusercontent.com/leijiadada/absorb/master/releases/latest.json"

// Config 吸收阅读助手 Go 服务的配置（key 仅存于此文件 + 环境变量，不进浏览器）
type Config struct {
	Model struct {
		Name        string         `yaml:"name"`
		Variant     string         `yaml:"variant"`
		APIKey      string         `yaml:"api_key"`
		BaseURL     string         `yaml:"base_url"`
		Temperature float64        `yaml:"temperature"`
		MaxTokens   int            `yaml:"max_tokens"`
		Thinking    ThinkingConfig `yaml:"thinking"`
	} `yaml:"model"`
	Store struct {
		Backend string `yaml:"backend"` // 存储后端：sqlite（默认）。预留 postgres/memory 等可插拔后端
		SQLite  struct {
			Path string `yaml:"path"`
		} `yaml:"sqlite"`
	} `yaml:"store"`
	Memory     MemoryConfig     `yaml:"memory"`
	Skills     SkillsConfig     `yaml:"skills"`
	Summary    SummaryConfig    `yaml:"summary"`
	WorkingSet WorkingSetConfig `yaml:"working_set"`
	Upgrade    UpgradeConfig    `yaml:"upgrade"`
	Proxy      ProxyConfig      `yaml:"proxy"`
	Fetch      FetchConfig      `yaml:"fetch"`
	Log        LogConfig        `yaml:"logs"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
}

// TelemetryConfig 生产可观测性（框架链路追踪）配置（默认关闭）。
// enabled 且 endpoint 为空：span 落盘 <logs.dir>/traces.jsonl（消息正文类属性
// 已按规则剔除）；sqlite=true 时改落会话库的 request_traces 表并开放
// /debug/traces 查看器；enabled 且 endpoint 非空：直发 OTLP（如本机 Jaeger），
// 不落盘（sqlite/debug_port 配置被忽略）。
type TelemetryConfig struct {
	Enabled     bool   `yaml:"enabled"`      // 总开关
	Endpoint    string `yaml:"endpoint"`     // OTLP endpoint（host:port，如 localhost:4317）；空=落盘/落库
	SQLite      bool   `yaml:"sqlite"`       // endpoint 为空时：true=落 request_traces 表（开放 /debug/traces），false=落 traces.jsonl
	DebugPort   int    `yaml:"debug_port"`   // sqlite 形态下把查看器固定挂 127.0.0.1:<port>（桌面桥端口动态分配，固定端口便于开发者收藏）；0=只挂业务 mux；被占则告警降级
	ServiceName string `yaml:"service_name"` // resource service.name，空=absorb
}

// WorkingSetConfig 工作集上下文配置：每轮请求前从会话库按全局 seq 取最近 N 个
// 完整问答轮次，经 agent.WithLateContextMessages 注入（不落库、逐 run 提供）。
// 不写 working_set 段 = 启用默认；enabled: false = 关闭。
type WorkingSetConfig struct {
	Enabled     *bool `yaml:"enabled"`       // 指针三态：未设置=启用，显式 false 关闭
	L1MaxRounds int   `yaml:"l1_max_rounds"` // 全局最近完整问答轮数上限，0=默认 2
	L1MaxTokens int   `yaml:"l1_max_tokens"` // L1 注入估算 token 预算，超限本轮可读报错（不裁剪），0=默认 16000
}

// On 返回开关有效值（未设置 = 启用）。
func (c WorkingSetConfig) On() bool { return c.Enabled == nil || *c.Enabled }

// L1Rounds 返回 L1 轮数有效值（0 = 默认 2）。
func (c WorkingSetConfig) L1Rounds() int {
	if c.L1MaxRounds > 0 {
		return c.L1MaxRounds
	}
	return 2
}

// L1Tokens 返回 L1 预算有效值（0 = 默认 16000）。
func (c WorkingSetConfig) L1Tokens() int {
	if c.L1MaxTokens > 0 {
		return c.L1MaxTokens
	}
	return 16000
}

// SummaryConfig 跨文章长会话摘要压缩配置（不写 summary 段即启用默认阈值）。
// 按文章 URL 分 scope 生成会话摘要：runner 每轮落库后异步触发，请求构建时按
// filterKey 注入摘要上下文。摘要模型跟随默认模型。
type SummaryConfig struct {
	Disabled       *bool `yaml:"disabled"`        // 显式 true 关闭（指针区分未设置与 false，默认启用）
	EventThreshold int   `yaml:"event_threshold"` // 距上次摘要的新事件数阈值（任一阈值达标即触发），0=默认 16
	TokenThreshold int   `yaml:"token_threshold"` // 距上次摘要的估算 token 阈值，0=默认 24000
	QueueSize      int   `yaml:"queue_size"`      // 摘要任务队列长度（满则丢弃该任务），0=默认 8
}

// Bool 返回 Disabled 的有效值（未设置 = false = 启用）。
func (c SummaryConfig) Bool() bool { return c.Disabled != nil && *c.Disabled }

// MemoryConfig 长期记忆配置（不写 memory 段或 enabled=false 即不启用）。
// 后端为框架 sqlite 子模块（replace 引入），auto 模式由 LLM 提取器在响应后异步写入。
type MemoryConfig struct {
	Enabled      bool          `yaml:"enabled"`       // 总开关
	DBPath       string        `yaml:"db_path"`       // 记忆库 SQLite 文件路径（独立于会话库）
	PreloadLimit int `yaml:"preload_limit"` // 已废弃：框架预加载由 related_memories 尾部注入替代，恒为 0（字段保留兼容旧 yaml）
	AsyncNum     int           `yaml:"async_num"`     // auto 提取并发 worker 数，0=默认 1
	QueueSize    int           `yaml:"queue_size"`    // 提取任务队列长度（满则降级同步），0=默认 10
	Profile      ProfileConfig `yaml:"profile"`       // L3 用户画像蒸馏（文件式，L1 之上周期蒸馏）
}

// ProfileConfig L3 画像蒸馏配置：后台 ticker 按水位（新增原子记忆条数）把 L1 facts
// 蒸馏为稳定画像文件，注入通道为 BuildUserMessage 顶部 <user_profile> 块。
type ProfileConfig struct {
	Enabled         bool   `yaml:"enabled"`          // 蒸馏开关（画像文件存在时即便关闭仍会被注入）
	IntervalMinutes int    `yaml:"interval_minutes"` // 蒸馏检查间隔，0=默认 10 分钟
	MinNewEntries   int    `yaml:"min_new_entries"`  // 触发蒸馏的新增记忆条数水位，0=默认 5
	FilePath        string `yaml:"file_path"`        // 画像文件路径，0=默认 ./data/profile.md
}

// UpgradeConfig 升级检查配置（不写 upgrade 段 = 默认启用自动检查）。
// 下载与安装始终需要用户确认（CLI 用 --yes），配置只能开/关"检查"，不能静默安装。
type UpgradeConfig struct {
	Enabled            *bool  `yaml:"enabled"`              // 总开关（指针三态：未设置=启用）
	Channel            string `yaml:"channel"`              // stable / beta，空=stable
	FeedURL            string `yaml:"feed_url"`             // latest.json 地址，空=默认仓库 raw 地址
	CheckIntervalHours int    `yaml:"check_interval_hours"` // 自动检查间隔小时，0=默认 6
	AutoCheck          *bool  `yaml:"auto_check"`           // 启动时后台检查（指针三态：未设置=启用）
	TempDir            string `yaml:"temp_dir"`             // 安装包暂存目录，空=%TEMP%\absorb-agent-upgrade
}

// On 升级功能总开关有效值（未设置 = 启用）。
func (c UpgradeConfig) On() bool { return c.Enabled == nil || *c.Enabled }

// AutoCheckOn 启动时自动检查有效值（未设置 = 启用）。
func (c UpgradeConfig) AutoCheckOn() bool { return c.AutoCheck == nil || *c.AutoCheck }

// ProxyConfig 全局出网代理（作用于 fetch 抓取、模型 API 调用等所有对外请求）。
// 空=跟随系统代理（Windows WinINET）/ 环境变量，未检测到则直连。
type ProxyConfig struct {
	URL string `yaml:"url"` // HTTP 代理地址（如 http://127.0.0.1:7897），空=跟随系统代理
}

// FetchConfig fetch_page 抓取配置（不写 fetch 段 = 默认 30s 超时 / 10MB 内容上限）。
type FetchConfig struct {
	TimeoutSeconds int `yaml:"timeout_seconds"` // 单请求超时秒数，0=默认 30
	MaxContentMB   int `yaml:"max_content_mb"`  // 单 URL 内容上限 MB，0=默认 10
}

// SkillsConfig Agent Skills 技能系统配置（不写 skills 段即不启用）。
type SkillsConfig struct {
	Root string `yaml:"root"` // 技能仓库根目录；空=不启用
}

// LogConfig 文件日志配置（zap 输出到 data/logs，轮转防膨胀）
type LogConfig struct {
	Dir        string `yaml:"dir"`          // 日志目录
	Level      string `yaml:"level"`        // debug/info/warn/error
	MaxSizeMB  int    `yaml:"max_size_mb"`  // 单文件上限（MB）
	MaxBackups int    `yaml:"max_backups"`  // 保留最多文件数
	MaxAgeDays int    `yaml:"max_age_days"` // 保留最多天数
}

// ThinkingConfig 思考模式配置(GLM-4.5+ 支持)
type ThinkingConfig struct {
	Enabled bool `yaml:"enabled"` // true→{"type":"enabled"} false→{"type":"disabled"}
	Tokens  int  `yaml:"tokens"`  // 0=不设思考 token 上限
}

// Handle 配置运行时句柄：读写共用一把 RWMutex，设置保存（写）与请求侧读取并发安全。
// Config 全为值类型字段，Get 返回快照副本。约定：运行时读配置一律走句柄，
// 新增"值类"配置（每次使用时读取）经句柄读取即天然热切换，无需额外处理。
type Handle struct {
	mu  sync.RWMutex
	cfg Config
}

// NewHandle 以初始配置构建句柄。
func NewHandle(cfg Config) *Handle { return &Handle{cfg: cfg} }

// Get 返回配置快照（值拷贝，外部修改不影响句柄内状态）。
func (h *Handle) Get() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Set 原子替换配置快照（调用方负责先落盘成功再 Set，保持内存态与磁盘一致）。
func (h *Handle) Set(cfg Config) {
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
}

// LoadConfig 读取并解析 config.yaml
func LoadConfig(path string) Config {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		panic("读取 config 失败: " + err.Error())
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		panic("解析 config 失败: " + err.Error())
	}
	if cfg.Model.Variant == "" {
		cfg.Model.Variant = "openai"
	}
	// MaxTokens <= 0 = 不限制输出（请求层不传该字段，上限由模型侧决定）
	if cfg.Model.Temperature == 0 {
		cfg.Model.Temperature = 0.7
	}
	if cfg.Store.SQLite.Path == "" {
		cfg.Store.SQLite.Path = "./data/sessions.db"
	}
	if cfg.Store.Backend == "" {
		cfg.Store.Backend = "sqlite"
	}
	if cfg.Memory.DBPath == "" {
		cfg.Memory.DBPath = "./data/memory.db"
	}
	// PreloadLimit 不再给默认值：框架预加载已由自研 related_memories 尾部注入替代（恒关闭）
	if cfg.Memory.AsyncNum == 0 {
		cfg.Memory.AsyncNum = 1
	}
	if cfg.Memory.QueueSize == 0 {
		cfg.Memory.QueueSize = 10
	}
	if cfg.Memory.Profile.IntervalMinutes == 0 {
		cfg.Memory.Profile.IntervalMinutes = 10
	}
	if cfg.Memory.Profile.MinNewEntries == 0 {
		cfg.Memory.Profile.MinNewEntries = 5
	}
	if cfg.Memory.Profile.FilePath == "" {
		cfg.Memory.Profile.FilePath = "./data/profile.md"
	}
	if cfg.Summary.EventThreshold == 0 {
		cfg.Summary.EventThreshold = 16
	}
	if cfg.Summary.TokenThreshold == 0 {
		cfg.Summary.TokenThreshold = 24000
	}
	if cfg.Summary.QueueSize == 0 {
		cfg.Summary.QueueSize = 8
	}
	if cfg.Fetch.TimeoutSeconds == 0 {
		cfg.Fetch.TimeoutSeconds = 30
	}
	if cfg.Fetch.MaxContentMB == 0 {
		cfg.Fetch.MaxContentMB = 10
	}
	if cfg.Upgrade.Channel == "" {
		cfg.Upgrade.Channel = "stable"
	}
	if cfg.Upgrade.FeedURL == "" {
		cfg.Upgrade.FeedURL = DefaultUpgradeFeedURL
	}
	if cfg.Upgrade.CheckIntervalHours == 0 {
		cfg.Upgrade.CheckIntervalHours = 6
	}
	if cfg.Log.Dir == "" {
		cfg.Log.Dir = "./logs"
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.MaxSizeMB == 0 {
		cfg.Log.MaxSizeMB = 10
	}
	if cfg.Log.MaxBackups == 0 {
		cfg.Log.MaxBackups = 5
	}
	if cfg.Log.MaxAgeDays == 0 {
		cfg.Log.MaxAgeDays = 7
	}
	if cfg.Telemetry.ServiceName == "" {
		cfg.Telemetry.ServiceName = "absorb"
	}
	return cfg
}
