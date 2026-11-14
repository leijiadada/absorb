// Package tokentracker 是 LLM token 用量的统计采集层（telemetry 第一批）。
//
// 职责：接收 chat/draft/summary 三类调用点的用量上报，做内存聚合（按本地日/kind/model
// 累加），并把原始流水交给 sink 落库（SQLite token_usage 表，由 main 装配注入）。
// 统计链路是旁路：聚合只在内存累加，落库经独立 goroutine 异步执行，失败仅记日志，
// 绝不影响问答主链路。
package tokentracker

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"absorb-agent/internal/log"
)

// Kind 用量上报的三类调用点（对应 token_usage.kind 列）。
const (
	KindChat    = "chat"    // 问答主链路（AskHandler 最终 assistant 事件）
	KindDraft   = "draft"   // 知识草稿生成（KnowledgeDraftFn）
	KindSummary = "summary" // 会话摘要（summarizer report hook）
)

// Usage 单次调用的 token 用量（字段映射框架 model.Usage，本包保持与框架类型解耦）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	TotalTokens      int
	CachedTokens     int // prompt 缓存命中（model.Usage.PromptTokensDetails.CachedTokens）
}

// Record 单次上报的原始流水（对应 token_usage 表一行）。
type Record struct {
	TS               time.Time
	Kind             string
	Model            string
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	TotalTokens      int
	CachedTokens     int
}

// UsageStat 一个 (本地日, kind, model) 维度的累计用量（Snapshot 返回，后续前端展示用）。
type UsageStat struct {
	Day              string `json:"day"`
	Kind             string `json:"kind"`
	Model            string `json:"model"`
	Calls            int    `json:"calls"`
	PromptTokens     int    `json:"promptTokens"`
	CompletionTokens int    `json:"completionTokens"`
	ReasoningTokens  int    `json:"reasoningTokens"`
	TotalTokens      int    `json:"totalTokens"`
	CachedTokens     int    `json:"cachedTokens"`
}

// bucketKey 内存聚合桶的维度键。
type bucketKey struct {
	day   string // 本地日期 YYYY-MM-DD
	kind  string
	model string
}

// Tracker 线程安全的内存聚合器：按 (日, kind, model) 维度累加，并可挂 sink 异步落库。
type Tracker struct {
	mu      sync.Mutex
	buckets map[bucketKey]*UsageStat
	sink    func(Record)     // 流水落库回调（main 装配；每条流水在独立 goroutine 中调用）
	now     func() time.Time // 时间源（测试注入用，默认 time.Now）
}

// NewTracker 构建空 Tracker。
func NewTracker() *Tracker {
	return &Tracker{buckets: map[bucketKey]*UsageStat{}, now: time.Now}
}

// defaultTracker 全局单例：main 启动装配 sink，各调用点直接使用包级入口。
var defaultTracker = NewTracker()

// SetSink 注入流水落库回调（main 启动装配；传 nil 清除）。落库失败由 sink 内部记日志。
func SetSink(fn func(Record)) {
	defaultTracker.mu.Lock()
	defaultTracker.sink = fn
	defaultTracker.mu.Unlock()
}

// Track 包级入口：向全局单例上报一次用量（每次上报打一条 INFO 日志）。
// 函数名未沿用 Record——Record 是流水结构体名，Go 不允许同名重载。
func Track(kind, mdl string, u Usage) { defaultTracker.record(kind, mdl, u) }

// Snapshot 返回全局聚合快照。
func Snapshot() []UsageStat { return defaultTracker.Snapshot() }

// Summary 返回全局今日用量的可读汇总。
func Summary() string { return defaultTracker.Summary() }

// record 累加聚合桶、打 INFO 日志，并异步交给 sink 落库。
func (t *Tracker) record(kind, mdl string, u Usage) {
	rec := Record{
		TS:               t.now(),
		Kind:             kind,
		Model:            mdl,
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		ReasoningTokens:  u.ReasoningTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.CachedTokens,
	}
	// 端点未回 total 时用 prompt+completion 兜底（summary 场景 completion 恒缺省，
	// 兜底后 total=prompt）。
	if rec.TotalTokens == 0 {
		rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	}

	key := bucketKey{day: rec.TS.In(time.Local).Format("2006-01-02"), kind: rec.Kind, model: rec.Model}
	t.mu.Lock()
	b := t.buckets[key]
	if b == nil {
		b = &UsageStat{Day: key.day, Kind: key.kind, Model: key.model}
		t.buckets[key] = b
	}
	b.Calls++
	b.PromptTokens += rec.PromptTokens
	b.CompletionTokens += rec.CompletionTokens
	b.ReasoningTokens += rec.ReasoningTokens
	b.TotalTokens += rec.TotalTokens
	b.CachedTokens += rec.CachedTokens
	sink := t.sink
	t.mu.Unlock()

	log.Infof("token-usage: kind=%s model=%s prompt=%d completion=%d total=%d cached=%d",
		rec.Kind, rec.Model, rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens, rec.CachedTokens)

	if sink != nil {
		// 异步落库：SQLite 写入耗时与全局写锁竞争都不传导回主链路；失败由 sink 记日志。
		go sink(rec)
	}
}

// Snapshot 返回当前聚合快照（按 day/kind/model 升序，输出稳定便于展示与测试）。
func (t *Tracker) Snapshot() []UsageStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]UsageStat, 0, len(t.buckets))
	for _, b := range t.buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Summary 今日（本地日）用量的一行式可读汇总：total / prompt / completion / cached。
func (t *Tracker) Summary() string {
	today := t.now().In(time.Local).Format("2006-01-02")
	t.mu.Lock()
	var prompt, completion, total, cached int
	for k, b := range t.buckets {
		if k.day != today {
			continue
		}
		prompt += b.PromptTokens
		completion += b.CompletionTokens
		total += b.TotalTokens
		cached += b.CachedTokens
	}
	t.mu.Unlock()
	return fmt.Sprintf("token-usage today: total=%d prompt=%d completion=%d cached=%d",
		total, prompt, completion, cached)
}
