// Package log 是应用统一的日志门面。
//
// 业务代码只依赖本包，底层由 trpc-agent-go 的 log 门面承载；
// 经 InitLogging 接管后写入轮转日志文件，不再输出到终端。
package log

import (
	stdlog "log"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
	tlog "trpc.group/trpc-go/trpc-agent-go/log"

	"absorb-agent/internal/conf"
)

// InitLogging 把日志收敛到单一文件：
//   - 接管 trpc-agent-go 框架日志（Default / ContextDefault）；
//   - 标准库 log.* 的输出一并重定向到同一文件。
//
// 由 lumberjack 按大小与天数轮转。幂等：重复调用只替换输出目标。
func InitLogging(lc conf.LogConfig) error {
	if err := os.MkdirAll(lc.Dir, 0o755); err != nil {
		return err
	}
	cleanupLegacyLogs()

	lvl, err := zapcore.ParseLevel(lc.Level)
	if err != nil {
		return err
	}

	w := &lumberjack.Logger{
		Filename:   filepath.Join(lc.Dir, "absorb.log"),
		MaxSize:    lc.MaxSizeMB,
		MaxBackups: lc.MaxBackups,
		MaxAge:     lc.MaxAgeDays,
		LocalTime:  true,
	}

	// 文件不打印 ANSI 颜色，用纯文本编码
	encCfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "lvl",
		NameKey:        "name",
		CallerKey:      "caller",
		MessageKey:     "message",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.RFC3339TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
	core := zapcore.NewCore(zapcore.NewConsoleEncoder(encCfg), zapcore.AddSync(w), lvl)

	appLogger := zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1)).Sugar()
	tlog.Default = appLogger
	tlog.ContextDefault = appLogger

	stdlog.SetOutput(w)
	stdlog.SetFlags(stdlog.LstdFlags | stdlog.Lmicroseconds)
	return nil
}

// cleanupLegacyLogs 删除旧版本遗留的 data/logs 目录。
// 日志已从 data/ 下移到根级 logs/，旧目录不会再被使用。
func cleanupLegacyLogs() {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	legacy := filepath.Join(wd, "data", "logs")
	if st, err := os.Stat(legacy); err == nil && st.IsDir() {
		_ = os.RemoveAll(legacy)
	}
}

// SetLevel 调整日志级别（级别名同 zapcore.ParseLevel）。
func SetLevel(level string) { tlog.SetLevel(level) }

// Debug 输出调试日志。
func Debug(args ...any) { tlog.Debug(args...) }

// Debugf 输出格式化的调试日志。
func Debugf(format string, args ...any) { tlog.Debugf(format, args...) }

// Info 输出常规日志。
func Info(args ...any) { tlog.Info(args...) }

// Infof 输出格式化的常规日志。
func Infof(format string, args ...any) { tlog.Infof(format, args...) }

// Warn 输出警告日志。
func Warn(args ...any) { tlog.Warn(args...) }

// Warnf 输出格式化的警告日志。
func Warnf(format string, args ...any) { tlog.Warnf(format, args...) }

// Error 输出错误日志。
func Error(args ...any) { tlog.Error(args...) }

// Errorf 输出格式化的错误日志。
func Errorf(format string, args ...any) { tlog.Errorf(format, args...) }

// Fatal 输出致命错误日志并退出进程。
func Fatal(args ...any) { tlog.Fatal(args...) }

// Fatalf 输出格式化的致命错误日志并退出进程。
func Fatalf(format string, args ...any) { tlog.Fatalf(format, args...) }
