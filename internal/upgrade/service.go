// Package upgrade 应用自升级：检查更新 → 通知 → 下载 → 校验 → 停机保全 → 拉起安装。
//
// 设计约束（详见 design/upgrade.md）：
//   - 本包只 import 标准库，不依赖任何 internal 业务包（chat/store/bridge/conf）；
//     配置、存储、UI 一律由外部经接口注入，依赖方向只有 app → upgrade。
//   - 数据迁移不在本包执行：迁移由新版本启动时在 store 侧完成（见 design/migration.md），
//     本包只在安装前调用 PreApplyHook 做备份保全。
//   - 绝不静默安装未确认产物：Apply 必须由用户动作或 CLI --yes 触发。
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultCheckInterval 默认自动检查间隔。
const DefaultCheckInterval = 6 * time.Hour

// 检查被跳过的原因（Result.Skipped）。
const (
	SkipDevBuild  = "dev-build"
	SkipThrottled = "throttled"
	SkipDisabled  = "disabled"
)

// DefaultDownloadDir 安装包暂存目录：放在系统临时目录，不污染用户数据目录。
func DefaultDownloadDir() string {
	return filepath.Join(os.TempDir(), "absorb-agent-upgrade")
}

// Host 升级服务所需的宿主信息（wiring 层提供，本包不 import main）。
type Host interface {
	Version() string // 当前版本；dev 构建返回 "dev"
	Channel() string // stable / beta
	DataDir() string // 用户数据根（节流戳所在）
}

// BuildInfo 可选接口：Host 实现时 status 子命令展示构建时间。
type BuildInfo interface{ BuildTime() string }

// Notifier 状态出口（UI 侧实现；CLI 用 stdout 替身）。nil 时全部静默。
type Notifier interface {
	OnUpdateAvailable(rel Release)
	OnDownloading(pct int)
	OnNoUpdate(current, latest string)
	OnError(err error)
}

// PreApplyHook 升级前保全（store 实现：备份）。返回错误即中止升级、不安装。
type PreApplyHook interface {
	BeforeApply(ctx context.Context, rel Release) error
}

// Option 函数式选项。
type Option func(*Service)

// Service 升级服务。Check 可并发；Apply 串行（applyMu 防并发安装）。
type Service struct {
	host     Host
	fetcher  FeedFetcher
	applier  Applier
	notifier Notifier
	preApply PreApplyHook
	dl       *Downloader
	throttle *Throttle
	now      func() time.Time

	feedURL   string
	channel   string
	interval  time.Duration
	autoCheck bool

	applyMu sync.Mutex

	// 最近一次检查结论（LastResult 读；Check/CheckThrottled 写）。
	resMu   sync.Mutex
	last    Result
	lastErr error
}

// Result 一次检查的结论。
type Result struct {
	Current   string   `json:"current"`
	Latest    string   `json:"latest"`
	Channel   string   `json:"channel"`
	Available bool     `json:"available"`         // 有新版本可升级
	Mandatory bool     `json:"mandatory"`         // 当前版本低于 feed 的 minimum_version
	Release   *Release `json:"release,omitempty"` // Available 时非 nil
	Skipped   string   `json:"skipped,omitempty"` // dev-build / throttled
}

// New 构造升级服务。未注入项使用默认实现：HTTP fetcher(30s)、平台 Applier、
// 暂存目录 DefaultDownloadDir()、间隔 DefaultCheckInterval。
func New(h Host, opts ...Option) *Service {
	s := &Service{
		host:      h,
		fetcher:   NewHTTPFetcher(30 * time.Second),
		applier:   newPlatformApplier(),
		dl:        &Downloader{Dir: DefaultDownloadDir()},
		interval:  DefaultCheckInterval,
		autoCheck: true,
		now:       time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	s.throttle = NewThrottle(h.DataDir(), s.interval, s.now)
	return s
}

// WithFeedURL 覆盖 feed 地址（空 = 未配置，Check 返回 ErrNoFeedURL）。
func WithFeedURL(u string) Option { return func(s *Service) { s.feedURL = u } }

// WithChannel 覆盖通道（与 feed 的 channel 不一致时按坏 feed 处理）。
func WithChannel(c string) Option { return func(s *Service) { s.channel = c } }

// WithInterval 覆盖自动检查间隔。
func WithInterval(d time.Duration) Option { return func(s *Service) { s.interval = d } }

// WithFetcher 替换 feed 拉取实现（测试注入）。
func WithFetcher(f FeedFetcher) Option { return func(s *Service) { s.fetcher = f } }

// WithApplier 替换安装实现（测试注入假安装器）。
func WithApplier(a Applier) Option { return func(s *Service) { s.applier = a } }

// WithNotifier 设置状态回调。
func WithNotifier(n Notifier) Option { return func(s *Service) { s.notifier = n } }

// WithPreApply 设置升级前保全钩子。
func WithPreApply(h PreApplyHook) Option { return func(s *Service) { s.preApply = h } }

// WithDownloadDir 覆盖安装包暂存目录。
func WithDownloadDir(dir string) Option { return func(s *Service) { s.dl.Dir = dir } }

// WithHTTPClient 注入 HTTP 客户端（feed 与下载共用；wiring 层用于接系统代理）。
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) {
		if f, ok := s.fetcher.(*HTTPFetcher); ok && c != nil {
			f.Client = c
		}
		if c != nil {
			s.dl.Client = c
		}
	}
}

// WithClock 注入时钟（测试用；节流器随之重建）。
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithAutoCheck 设置启动时自动检查开关（配置关闭时 CheckAndNotify 直接跳过）。
func WithAutoCheck(on bool) Option { return func(s *Service) { s.autoCheck = on } }

// FeedURL / Channel / Interval / DataDir 访问器（status 子命令与 wiring 用）。
func (s *Service) FeedURL() string         { return s.feedURL }
func (s *Service) Channel() string         { return s.channel }
func (s *Service) Interval() time.Duration { return s.interval }
func (s *Service) DataDir() string         { return s.host.DataDir() }
func (s *Service) CurrentVersion() string  { return s.host.Version() }

// LastCheck 上次成功检查时间（无记录返回 false）。
func (s *Service) LastCheck() (time.Time, bool) { return s.throttle.Last() }

// With 返回应用了额外选项的副本（CLI --feed/--channel 覆盖用，不影响原服务）。
// 逐字段构造而非整体拷贝：Service 内含 sync.Mutex，禁止拷贝锁。
func (s *Service) With(opts ...Option) *Service {
	clone := &Service{
		host:      s.host,
		fetcher:   s.fetcher,
		applier:   s.applier,
		notifier:  s.notifier,
		preApply:  s.preApply,
		throttle:  s.throttle,
		now:       s.now,
		feedURL:   s.feedURL,
		channel:   s.channel,
		interval:  s.interval,
		autoCheck: s.autoCheck,
	}
	if s.dl != nil {
		d := *s.dl
		clone.dl = &d
	}
	for _, o := range opts {
		o(clone)
	}
	return clone
}

// Check 强制检查（忽略节流），并记录为最近一次结论（LastResult 可读）。
//   - 已最新/当前不低于 feed：返回 ErrNoUpdate（Available=false）；
//   - dev 构建：返回 ErrDevBuild（Skipped=dev-build）；
//   - 有新版本：err=nil，Available=true（Mandatory 见 minimum_version）。
func (s *Service) Check(ctx context.Context) (Result, error) {
	res, err := s.check(ctx)
	err = Coded(err)
	s.storeResult(res, err)
	return res, err
}

// check 实际检查逻辑（不写 last 状态，供 Check/CheckThrottled 复用）。
func (s *Service) check(ctx context.Context) (Result, error) {
	cur := s.host.Version()
	cv, err := ParseVersion(cur)
	if err != nil {
		return Result{Current: cur}, err
	}
	if cv.Dev {
		return Result{Current: cur, Skipped: SkipDevBuild}, ErrDevBuild
	}
	if s.feedURL == "" {
		return Result{Current: cur}, ErrNoFeedURL
	}
	rel, err := s.fetcher.Fetch(ctx, s.feedURL)
	if err != nil {
		return Result{Current: cur}, err
	}
	if s.channel != "" && rel.Channel != "" && rel.Channel != s.channel {
		return Result{Current: cur}, fmt.Errorf("%w: feed channel %q != configured %q", ErrBadFeed, rel.Channel, s.channel)
	}
	rv, err := ParseVersion(rel.Version)
	if err != nil {
		return Result{Current: cur}, err
	}
	out := Result{Current: cur, Latest: rel.Version, Channel: rel.Channel, Release: &rel}
	if rv.Compare(cv) <= 0 {
		return out, fmt.Errorf("%w: current %s >= feed %s", ErrNoUpdate, cur, rel.Version)
	}
	out.Available = true
	if rel.MinimumVersion != "" {
		if mv, err := ParseVersion(rel.MinimumVersion); err == nil && cv.Compare(mv) < 0 {
			out.Mandatory = true
		}
	}
	return out, nil
}

// LastResult 最近一次检查结论（未检查过时返回零值 Result 与 nil error）。
func (s *Service) LastResult() (Result, error) {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	return s.last, s.lastErr
}

func (s *Service) storeResult(res Result, err error) {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	s.last, s.lastErr = res, err
}

// CheckThrottled 带节流的检查（桌面启动时用）。间隔内跳过返回 ErrThrottled；
// 仅当真正访问到 feed（有新版本或已最新）才写时间戳，网络失败下次继续重试。
func (s *Service) CheckThrottled(ctx context.Context) (Result, error) {
	cur := s.host.Version()
	if !s.autoCheck {
		return Result{Current: cur, Skipped: SkipDisabled}, Coded(ErrDisabled)
	}
	if !s.throttle.Allow() {
		return Result{Current: cur, Skipped: SkipThrottled}, Coded(ErrThrottled)
	}
	res, err := s.check(ctx)
	err = Coded(err)
	s.storeResult(res, err)
	if err == nil || errors.Is(err, ErrNoUpdate) {
		_ = s.throttle.Touch()
	}
	return res, err
}

// CheckAndNotify 查询并按结果回调 Notifier。配置关闭、节流跳过与 dev 构建均静默。
func (s *Service) CheckAndNotify(ctx context.Context) (Result, error) {
	res, err := s.CheckThrottled(ctx)
	switch {
	case err == nil:
		s.notify(func(n Notifier) { n.OnUpdateAvailable(*res.Release) })
	case errors.Is(err, ErrNoUpdate):
		s.notify(func(n Notifier) { n.OnNoUpdate(res.Current, res.Latest) })
	case errors.Is(err, ErrThrottled), errors.Is(err, ErrDevBuild), errors.Is(err, ErrDisabled):
		// 静默：不该打扰用户
	default:
		s.notifyErr(err)
	}
	return res, err
}

// Apply 下载 → 校验 SHA256 → PreApplyHook 保全 → 拉起安装器。
// 任一步失败即中止，且校验失败的安装包会被删除；成功后安装器自行结束旧进程并重启。
func (s *Service) Apply(ctx context.Context, rel Release) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	art, err := rel.PickArtifact(platform())
	if err != nil {
		c := Coded(err)
		s.notifyErr(c)
		return c
	}
	s.notify(func(n Notifier) { n.OnDownloading(0) })

	// 下载进度转成百分比通知（保留调用方通过 WithDownloadDir 等设置的原始回调）。
	prevProgress := s.dl.Progress
	s.dl.Progress = func(done, total int64) {
		if prevProgress != nil {
			prevProgress(done, total)
		}
		s.notify(func(n Notifier) { n.OnDownloading(pct(done, total)) })
	}
	pkg, err := s.dl.Fetch(ctx, *art)
	s.dl.Progress = prevProgress
	if err != nil {
		c := WithCode("download_fail", err)
		s.notifyErr(c)
		return c
	}
	s.notify(func(n Notifier) { n.OnDownloading(100) })

	if s.preApply != nil {
		if err := s.preApply.BeforeApply(ctx, rel); err != nil {
			c := WithCode("backup_fail", fmt.Errorf("pre-apply backup: %w", err))
			s.notifyErr(c)
			return c
		}
	}
	if err := s.applier.Install(ctx, pkg, rel); err != nil {
		c := Coded(err)
		s.notifyErr(c)
		return c
	}
	return nil
}

// notify  nil 安全的 Notifier 调用。
func (s *Service) notify(f func(Notifier)) {
	if s.notifier != nil {
		f(s.notifier)
	}
}

func (s *Service) notifyErr(err error) {
	s.notify(func(n Notifier) { n.OnError(err) })
}

// pct 下载进度百分比（total 未知时返回 0）。
func pct(done, total int64) int {
	if total <= 0 {
		return 0
	}
	if done >= total {
		return 100
	}
	return int(done * 100 / total)
}
