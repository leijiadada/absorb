package upgrade

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Throttle 自动检查节流：把最近一次「成功查到 feed」的时间戳写在
// <DataDir>/upgrade/last_check，间隔内跳过自动检查。
// 手动检查（CLI check / 用户点按钮）不读节流，始终强制执行。
type Throttle struct {
	dir      string
	interval time.Duration
	now      func() time.Time
	mu       sync.Mutex
}

// NewThrottle 构造节流器。dir 为空（如内存测试）时退化为不节流。
func NewThrottle(dir string, interval time.Duration, now func() time.Time) *Throttle {
	if now == nil {
		now = time.Now
	}
	return &Throttle{dir: dir, interval: interval, now: now}
}

func (t *Throttle) stampPath() string { return filepath.Join(t.dir, "upgrade", "last_check") }

// Last 返回上次成功检查时间；无记录返回 false。
func (t *Throttle) Last() (time.Time, bool) {
	if t == nil || t.dir == "" {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b, err := os.ReadFile(t.stampPath())
	if err != nil {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// Allow 是否允许执行一次自动检查。
func (t *Throttle) Allow() bool {
	last, ok := t.Last()
	if !ok {
		return true
	}
	return t.now().Sub(last) >= t.interval
}

// Touch 记录本次成功检查的时间。
func (t *Throttle) Touch() error {
	if t == nil || t.dir == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(t.stampPath()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(t.stampPath(), []byte(strconv.FormatInt(t.now().Unix(), 10)), 0o600)
}
