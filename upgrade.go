package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
	"absorb-agent/internal/upgrade"
	fetch "absorb-agent/tool/fetch_page"
)

// 升级模块的 wiring 层：把 conf / store / wails 适配成 upgrade 包所需的接口。
// internal/upgrade 本身只依赖标准库（见 arch_test.go 的 import 白名单断言），
// 所有与主程序的耦合都收敛在本文件。

// ---------- Host ----------

// upgradeHost 实现 upgrade.Host 与 upgrade.BuildInfo：
// 版本取 main.desktopVersion（ldflags 注入），构建时间取 main.builtAt。
type upgradeHost struct {
	version string
	builtAt string
	channel string
	dataDir string
}

func (h upgradeHost) Version() string   { return h.version }
func (h upgradeHost) Channel() string   { return h.channel }
func (h upgradeHost) DataDir() string   { return h.dataDir }
func (h upgradeHost) BuildTime() string { return h.builtAt }

// ---------- 数据层保全 / 状态 ----------

// storeMaintainer 把 store.SessionService 适配为 upgrade.DataMaintainer。
type storeMaintainer struct{ svc *store.SessionService }

func (m storeMaintainer) BackupNow(ctx context.Context) (string, error) {
	return m.svc.BackupNow(ctx)
}

// BeforeApply 升级前保全：只做一致性备份（不执行数据迁移——迁移由新版本启动时完成）。
func (m storeMaintainer) BeforeApply(ctx context.Context, rel upgrade.Release) error {
	path, err := m.svc.BackupNow(ctx)
	if err != nil {
		return err
	}
	if path != "" {
		log.Infof("[Absorb] pre-upgrade backup created: %s (target %s)", path, rel.Version)
	}
	return nil
}

func (m storeMaintainer) SchemaInfo(ctx context.Context) (upgrade.SchemaInfo, error) {
	info, err := m.svc.SchemaInfo(ctx)
	if err != nil {
		return upgrade.SchemaInfo{}, err
	}
	return upgrade.SchemaInfo{Version: info.Version, Backups: info.Backups}, nil
}

// ---------- Notifier ----------

// cliNotifier CLI 形态：状态打到 stdout（Runner 自己负责结构化输出，这里只作补充）。
type cliNotifier struct{ out io.Writer }

func (n cliNotifier) OnUpdateAvailable(rel upgrade.Release) {
	fmt.Fprintf(n.out, "发现新版本 %s\n", rel.Version)
}
func (n cliNotifier) OnDownloading(pct int) {
	fmt.Fprintf(n.out, "下载中 %d%%\n", pct)
}
func (n cliNotifier) OnNoUpdate(cur, _ string) {
	fmt.Fprintf(n.out, "已是最新版本（%s）\n", cur)
}
func (n cliNotifier) OnError(err error) { fmt.Fprintf(n.out, "升级检查失败: %v\n", err) }

// updateNotifier 桌面形态：经 Wails 事件推给前端（前端订阅 absorb:update-available /
// absorb:update-progress）。ctx 由 startup 注入，事件只在窗口就绪后发出。
type updateNotifier struct{ app *App }

func (n updateNotifier) OnUpdateAvailable(rel upgrade.Release) {
	runtime.EventsEmit(n.app.ctx, "absorb:update-available", map[string]any{
		"version":         rel.Version,
		"minimum_version": rel.MinimumVersion,
		"notes_url":       rel.NotesURL,
	})
}
func (n updateNotifier) OnDownloading(pct int) {
	runtime.EventsEmit(n.app.ctx, "absorb:update-progress", map[string]any{"percent": pct})
}
func (n updateNotifier) OnNoUpdate(string, string) {} // 启动时静默：不该打扰
func (n updateNotifier) OnError(err error) {
	log.Warnf("[Absorb] update check failed: %v", err)
}

// ---------- 装配 ----------

// newUpgradeService 装配升级服务（CLI 与桌面共用；差异仅在注入的 Notifier/Hook）。
func newUpgradeService(cfg conf.Config, host upgrade.Host, opts ...upgrade.Option) *upgrade.Service {
	base := []upgrade.Option{
		upgrade.WithFeedURL(cfg.Upgrade.FeedURL),
		upgrade.WithChannel(cfg.Upgrade.Channel),
		upgrade.WithInterval(time.Duration(cfg.Upgrade.CheckIntervalHours) * time.Hour),
		upgrade.WithAutoCheck(cfg.Upgrade.On() && cfg.Upgrade.AutoCheckOn()),
		upgrade.WithHTTPClient(newUpgradeHTTPClient(cfg)),
	}
	if dir := strings.TrimSpace(cfg.Upgrade.TempDir); dir != "" {
		base = append(base, upgrade.WithDownloadDir(dir))
	}
	return upgrade.New(host, append(base, opts...)...)
}

// newUpgradeHTTPClient 升级链路 HTTP 客户端：跟随配置代理，否则跟随系统代理（与 fetch 一致）。
func newUpgradeHTTPClient(cfg conf.Config) *http.Client {
	proxyURL := strings.TrimSpace(cfg.Proxy.URL)
	if proxyURL == "" {
		if server, _, ok := fetch.SystemProxySnapshot(); ok {
			proxyURL = server
		}
	}
	transport := &http.Transport{}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	// Timeout 必须在此兜底：WithHTTPClient 会整体替换 internal/upgrade 默认客户端
	// （feed 检查 30s / 下载 10min），注入端不设超时则升级链路可无限挂起；
	// 取 15min 覆盖下载预算，feed 短请求实际远低于此。
	return &http.Client{Timeout: 15 * time.Minute, Transport: transport}
}

// ---------- CLI 入口 ----------

// runUpgradeCLI absorb-agent-cli upgrade <sub>：与桌面共用 upgrade.Service，
// 数据层保全与状态经 storeMaintainer 注入，可在无头环境自测升级全流程。
func runUpgradeCLI(args []string) int {
	cfgPath := "config/config.yaml"
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// 便携/安装布局：配置位于数据根（resolveDataRoot 会 chdir 过去）
		cfgPath = filepath.Join(resolveDataRoot(), "config", "config.yaml")
	}
	ensureDefaultConfig(cfgPath)
	cfg := conf.LoadConfig(cfgPath)

	host := upgradeHost{
		version: desktopVersion,
		builtAt: builtAt,
		channel: cfg.Upgrade.Channel,
		dataDir: upgradeDataDir(cfg),
	}
	svc := newUpgradeService(cfg, host, upgrade.WithNotifier(cliNotifier{out: os.Stdout}))

	// status / apply 需要数据层信息：打开会话库（顺带执行 schema 迁移，可验证迁移链路）。
	// 打不开必须上报——静默降级会让 status 显示"正常"，更糟的是让 apply 在无法备份的情况下升级。
	var maintainer upgrade.DataMaintainer
	var dataErr error
	if dbPath, err := filepath.Abs(cfg.Store.SQLite.Path); err != nil {
		dataErr = fmt.Errorf("resolve db path: %w", err)
	} else if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		dataErr = fmt.Errorf("create db dir: %w", err)
	} else if sess, err := store.New(cfg.Store.Backend, dbPath); err != nil {
		dataErr = fmt.Errorf("open storage: %w", err)
	} else {
		defer func() { _ = sess.Close() }()
		maintainer = upgrade.DataMaintainer(storeMaintainer{svc: sess})
	}

	runner := &upgrade.Runner{
		Svc:        svc,
		Maintainer: maintainer,
		DataErr:    dataErr,
		AutoCheck:  cfg.Upgrade.On() && cfg.Upgrade.AutoCheckOn(),
		Out:        os.Stdout,
		ErrOut:     os.Stderr,
	}
	return runner.Run(args)
}

// upgradeDataDir 节流戳目录：取会话库所在目录（dev 的 ./data 与安装布局的数据根一致）。
func upgradeDataDir(cfg conf.Config) string {
	if p, err := filepath.Abs(cfg.Store.SQLite.Path); err == nil {
		return filepath.Dir(p)
	}
	return "."
}
