package upgrade

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// 退出码约定（可脚本断言）：
//
//	0  成功 / 已是最新 / 无需操作
//	1  失败（网络、校验、保全、安装）
//	10 check 专有：有新版本可用
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUpdateAvail = 10
)

// SchemaInfo 数据层自描述（status 展示用；由 wiring 层从 store 适配而来）。
type SchemaInfo struct {
	Version int      `json:"schema_version"`
	Backups []string `json:"backups,omitempty"`
}

// DataMaintainer 数据层保全能力：apply 前备份 + status 展示 schema 版本与备份列表。
type DataMaintainer interface {
	BackupNow(ctx context.Context) (string, error)
	SchemaInfo(ctx context.Context) (SchemaInfo, error)
}

// Runner 升级 CLI（main.go 装配后调用；absorb-agent-cli upgrade <sub>）。
type Runner struct {
	Svc        *Service
	Maintainer DataMaintainer           // 可选：status/apply 需要数据层信息时注入
	DataErr    error                    // 数据层不可用时的错误（status 展示 / apply 中止）；nil = 可用
	AutoCheck  bool                     // 是否启用启动时自动检查（仅 status 展示）
	Out        io.Writer                // nil = os.Stdout
	ErrOut     io.Writer                // nil = os.Stderr
	Stdin      io.Reader                // 交互确认输入；nil = os.Stdin（测试可注入）
	Confirm    func(prompt string) bool // 交互确认；nil = 读 Stdin
}

// dataLayerBroken 数据层被声明为不可用（与"未配置数据层"区分：后者仅测试会用）。
func (r *Runner) dataLayerBroken() bool { return r.DataErr != nil }

// Run 执行子命令并返回退出码。
func (r *Runner) Run(args []string) int {
	if len(args) == 0 {
		r.usage()
		return ExitError
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "check":
		return r.cmdCheck(rest)
	case "status":
		return r.cmdStatus(rest)
	case "apply":
		return r.cmdApply(rest)
	case "help", "-h", "--help":
		r.usage()
		return ExitOK
	default:
		fmt.Fprintf(r.errOut(), "unknown subcommand %q\n\n", sub)
		r.usage()
		return ExitError
	}
}

func (r *Runner) usage() {
	fmt.Fprint(r.out(), `absorb-agent-cli upgrade <sub> [flags]

子命令:
  check    查询 feed 并输出版本对比（--json 机器可读）
  status   输出版本/构建时间/通道/feed/节流状态/schema 版本/迁移备份
  apply    检查 → 下载 → 校验 → 备份保全 → 拉起安装程序

通用 flag:
  --json            JSON 输出
  --feed <url>      覆盖 feed 地址（默认取配置）
  --channel <c>     覆盖通道（stable|beta）
  --yes             apply 免交互确认

退出码: 0 成功/已最新 · 1 失败 · 10 check 专有：有新版本
`)
}

// out/errOut 输出口（nil 时落系统默认）。
func (r *Runner) out() io.Writer {
	if r.Out != nil {
		return r.Out
	}
	return os.Stdout
}
func (r *Runner) errOut() io.Writer {
	if r.ErrOut != nil {
		return r.ErrOut
	}
	return os.Stderr
}

// ---------------- check ----------------

type checkView struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Channel   string    `json:"channel,omitempty"`
	Available bool      `json:"available"`
	Mandatory bool      `json:"mandatory"`
	Artifact  *Artifact `json:"artifact,omitempty"`
	NotesURL  string    `json:"notes_url,omitempty"`
	Skipped   string    `json:"skipped,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func (r *Runner) cmdCheck(rest []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	var asJSON bool
	var feed, channel string
	fs.BoolVar(&asJSON, "json", false, "JSON 输出")
	fs.StringVar(&feed, "feed", "", "覆盖 feed 地址")
	fs.StringVar(&channel, "channel", "", "覆盖通道")
	if err := fs.Parse(rest); err != nil {
		return ExitError
	}
	svc := r.service(feed, channel)

	res, err := svc.Check(context.Background())
	view := checkView{
		Current: res.Current,
		Latest:  res.Latest,
		Channel: res.Channel,
		Skipped: res.Skipped,
	}
	switch {
	case errors.Is(err, ErrDevBuild):
		view.Error = "dev build"
		if asJSON {
			return r.printJSON(view)
		}
		fmt.Fprintf(r.out(), "当前为开发构建（%s），跳过版本比较。\n", res.Current)
		return ExitOK
	case errors.Is(err, ErrNoUpdate):
		if asJSON {
			return r.printJSON(view)
		}
		fmt.Fprintf(r.out(), "已是最新版本（%s）。\n", res.Current)
		return ExitOK
	case err != nil:
		view.Error = err.Error()
		if asJSON {
			r.printJSON(view)
		} else {
			fmt.Fprintf(r.errOut(), "检查更新失败: %v\n", err)
		}
		return ExitError
	}

	view.Available = true
	view.Mandatory = res.Mandatory
	if res.Release != nil {
		view.NotesURL = res.Release.NotesURL
		if art, aerr := res.Release.PickArtifact(platform()); aerr == nil {
			a := *art
			view.Artifact = &a
		}
	}
	if asJSON {
		return r.printJSON(view)
	}
	fmt.Fprintf(r.out(), "当前版本: %s\n最新版本: %s", res.Current, res.Latest)
	if res.Channel != "" {
		fmt.Fprintf(r.out(), " (%s)", res.Channel)
	}
	fmt.Fprintln(r.out())
	if res.Release != nil && !res.Release.PublishedAt.IsZero() {
		fmt.Fprintf(r.out(), "发布时间: %s\n", res.Release.PublishedAt.Format(time.RFC3339))
	}
	if view.Artifact != nil {
		fmt.Fprintf(r.out(), "安装包: %s (%s)\n", view.Artifact.Name, humanBytes(view.Artifact.Size))
	}
	if view.NotesURL != "" {
		fmt.Fprintf(r.out(), "发布说明: %s\n", view.NotesURL)
	}
	if res.Mandatory {
		min := ""
		if res.Release != nil {
			min = res.Release.MinimumVersion
		}
		fmt.Fprintf(r.out(), "⚠ 强制升级：当前版本低于最低要求 %s，请尽快升级。\n", min)
	}
	fmt.Fprintln(r.out(), "执行 absorb-agent-cli upgrade apply 开始升级。")
	return ExitUpdateAvail
}

// ---------------- status ----------------

type statusView struct {
	Version       string   `json:"version"`
	BuildTime     string   `json:"build_time,omitempty"`
	Channel       string   `json:"channel"`
	FeedURL       string   `json:"feed_url"`
	AutoCheck     bool     `json:"auto_check"`
	IntervalHours float64  `json:"interval_hours"`
	LastCheck     string   `json:"last_check,omitempty"`
	SchemaVersion int      `json:"schema_version,omitempty"`
	Backups       []string `json:"backups,omitempty"`
	DownloadDir   string   `json:"download_dir"`
}

func (r *Runner) cmdStatus(rest []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	var asJSON bool
	var feed, channel string
	fs.BoolVar(&asJSON, "json", false, "JSON 输出")
	fs.StringVar(&feed, "feed", "", "覆盖 feed 地址")
	fs.StringVar(&channel, "channel", "", "覆盖通道")
	if err := fs.Parse(rest); err != nil {
		return ExitError
	}
	svc := r.service(feed, channel)

	view := statusView{
		Version:       svc.CurrentVersion(),
		Channel:       svc.Channel(),
		FeedURL:       svc.FeedURL(),
		AutoCheck:     r.AutoCheck,
		IntervalHours: svc.Interval().Hours(),
		DownloadDir:   DefaultDownloadDir(),
	}
	if bi, ok := r.Svc.host.(BuildInfo); ok {
		view.BuildTime = bi.BuildTime()
	}
	if last, ok := svc.LastCheck(); ok {
		view.LastCheck = last.Format(time.RFC3339)
	}
	if r.Maintainer != nil {
		if info, err := r.Maintainer.SchemaInfo(context.Background()); err == nil {
			view.SchemaVersion = info.Version
			view.Backups = info.Backups
		}
	}
	if asJSON {
		return r.printJSON(view)
	}

	fmt.Fprintf(r.out(), "版本: %s", view.Version)
	if view.BuildTime != "" {
		fmt.Fprintf(r.out(), "（构建于 %s）", view.BuildTime)
	}
	fmt.Fprintln(r.out())
	fmt.Fprintf(r.out(), "通道: %s\nFeed: %s\n", view.Channel, view.FeedURL)
	if view.AutoCheck {
		fmt.Fprintf(r.out(), "自动检查: 开（间隔 %.0f 小时", view.IntervalHours)
		if view.LastCheck != "" {
			fmt.Fprintf(r.out(), "，上次检查 %s", view.LastCheck)
		}
		fmt.Fprintln(r.out(), "）")
	} else {
		fmt.Fprintln(r.out(), "自动检查: 关（仅手动 check 生效）")
	}
	fmt.Fprintf(r.out(), "安装包暂存: %s\n", view.DownloadDir)
	if r.dataLayerBroken() {
		// 数据层不可用必须显式报错：静默省略会让 status 看起来"正常"，
		// 更危险的是让 apply 误以为可以做升级前备份。
		fmt.Fprintf(r.errOut(), "数据层不可用: %v\n", r.DataErr)
		return ExitError
	}
	if r.Maintainer != nil {
		fmt.Fprintf(r.out(), "数据 schema 版本: %d\n", view.SchemaVersion)
		if len(view.Backups) == 0 {
			fmt.Fprintln(r.out(), "迁移备份: 无")
		} else {
			fmt.Fprintln(r.out(), "迁移备份（新→旧）:")
			for _, b := range view.Backups {
				fmt.Fprintf(r.out(), "  - %s\n", b)
			}
		}
	}
	return ExitOK
}

// ---------------- apply ----------------

type applyView struct {
	Target   string    `json:"target"`
	Current  string    `json:"current"`
	Artifact *Artifact `json:"artifact,omitempty"`
	Backup   string    `json:"backup,omitempty"`
	Launched bool      `json:"launched"`
	Error    string    `json:"error,omitempty"`
}

func (r *Runner) cmdApply(rest []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(r.errOut())
	var asJSON, yes bool
	var feed, channel string
	fs.BoolVar(&asJSON, "json", false, "JSON 输出")
	fs.BoolVar(&yes, "yes", false, "免交互确认")
	fs.StringVar(&feed, "feed", "", "覆盖 feed 地址")
	fs.StringVar(&channel, "channel", "", "覆盖通道")
	if err := fs.Parse(rest); err != nil {
		return ExitError
	}
	svc := r.service(feed, channel)

	// 数据层不可用时拒绝升级：备份是升级前的硬前提，不能静默跳过。
	if r.dataLayerBroken() {
		view := applyView{Current: svc.CurrentVersion(), Error: r.DataErr.Error()}
		return r.failApply(asJSON, view, "数据层不可用，拒绝在无法备份的情况下升级: "+r.DataErr.Error())
	}

	res, err := svc.Check(context.Background())
	view := applyView{Current: res.Current}
	switch {
	case errors.Is(err, ErrDevBuild):
		view.Error = "dev build cannot be upgraded"
		return r.failApply(asJSON, view, "当前为开发构建，无法通过 feed 升级。")
	case errors.Is(err, ErrNoUpdate):
		if asJSON {
			return r.printJSON(view)
		}
		fmt.Fprintf(r.out(), "已是最新版本（%s），无需升级。\n", res.Current)
		return ExitOK
	case err != nil:
		view.Error = err.Error()
		return r.failApply(asJSON, view, "检查更新失败: "+err.Error())
	}

	view.Target = res.Latest
	art, aerr := res.Release.PickArtifact(platform())
	if aerr != nil {
		view.Error = aerr.Error()
		return r.failApply(asJSON, view, aerr.Error())
	}
	view.Artifact = art

	if !asJSON {
		fmt.Fprintf(r.out(), "目标版本: %s（当前 %s）\n", res.Latest, res.Current)
		fmt.Fprintf(r.out(), "安装包: %s (%s)\n", art.Name, humanBytes(art.Size))
		if res.Release.NotesURL != "" {
			fmt.Fprintf(r.out(), "发布说明: %s\n", res.Release.NotesURL)
		}
	}
	if !yes && !r.confirm(fmt.Sprintf("确认下载并安装 %s？[y/N] ", res.Latest)) {
		if asJSON {
			view.Error = "aborted by user"
			r.printJSON(view)
		} else {
			fmt.Fprintln(r.out(), "已取消。")
		}
		return ExitError
	}

	if err := svc.Apply(context.Background(), *res.Release); err != nil {
		view.Error = err.Error()
		return r.failApply(asJSON, view, "升级失败: "+err.Error())
	}
	view.Launched = true
	if asJSON {
		return r.printJSON(view)
	}
	fmt.Fprintln(r.out(), "安装程序已启动；本进程随即退出，安装完成后应用会自动重启。")
	return ExitOK
}

// ---------------- helpers ----------------

// service 应用 --feed/--channel 覆盖（无覆盖时返回原服务）。
func (r *Runner) service(feed, channel string) *Service {
	var opts []Option
	if feed != "" {
		opts = append(opts, WithFeedURL(feed))
	}
	if channel != "" {
		opts = append(opts, WithChannel(channel))
	}
	if len(opts) == 0 {
		return r.Svc
	}
	return r.Svc.With(opts...)
}

// confirm 交互确认：优先注入的 Confirm，否则读 Stdin（EOF/非 y 均视为拒绝）。
func (r *Runner) confirm(prompt string) bool {
	if r.Confirm != nil {
		return r.Confirm(prompt)
	}
	fmt.Fprint(r.out(), prompt)
	in := r.Stdin
	if in == nil {
		in = os.Stdin
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func (r *Runner) failApply(asJSON bool, view applyView, msg string) int {
	if asJSON {
		r.printJSON(view)
	} else {
		fmt.Fprintln(r.errOut(), msg)
	}
	return ExitError
}

func (r *Runner) printJSON(v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(r.errOut(), "marshal json: %v\n", err)
		return ExitError
	}
	fmt.Fprintln(r.out(), string(b))
	// check 的退出码语义在 JSON 模式下同样成立（可供脚本判定）。
	if ev, ok := v.(checkView); ok {
		switch {
		case ev.Skipped != "":
			return ExitOK // dev-build 等跳过不是失败（与文本模式一致）
		case ev.Error != "":
			return ExitError
		case ev.Available:
			return ExitUpdateAvail
		}
	}
	return ExitOK
}

// humanBytes 人类可读字节数。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
