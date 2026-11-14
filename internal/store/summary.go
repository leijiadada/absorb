package store

import (
	"context"
	"strings"
	"sync"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
	summary "trpc.group/trpc-go/trpc-agent-go/session/summary"

	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
)

// 会话摘要压缩：
//   - EnqueueSummaryJob：runner 每轮落库 assistant/tool-result 事件后自动调用
//     （无条件入队，阈值 gating 在本文件 worker 内）；有界去重队列 + 单 worker
//     全局串行，同会话摘要天然互斥；
//   - CreateSessionSummary：按 filterKey 分 scope 过滤事件 → LLM 摘要 → 持久化
//     summaries 列（Boundary 随行 per-scope 持久化，作为该 scope 的增量边界）；
//   - GetSessionSummaryText：runner 构建请求时按 filterKey 读取摘要注入上下文；
//     无摘要返回 ("" ,false) → runner 走全量原始事件（降级安全）。
//
// 阈值判定在 worker 内自实现（scope 感知）：SDK 公开 Checker 在会话无 scope 元数据时
// 只统计 FilterKey=="" 的事件，无法正确计数 URL 打标的事件，而 scope 元数据注入
// 依赖 SDK 内部包，故不采用。

// summaryJobTimeout 单个摘要任务上限（含 LLM 调用）。
const summaryJobTimeout = 2 * time.Minute

// 落盘稳定等待：SDK runner 会在 run 进行中（最终回答落库前）入队摘要任务，
// worker 立即处理会漏掉轮尾事件。处理前轮询等待会话以"最终 assistant 文本"
// 收尾（run 完成的标志），有重试上限兜底（被取消的悬挂轮永远等不到）。
const (
	summarySettleDelay    = 1500 * time.Millisecond
	summarySettleAttempts = 3
)

// endsWithFinalAssistant 会话事件是否以最终 assistant 文本收尾（run 完成的标志）。
func endsWithFinalAssistant(events []event.Event) bool {
	if len(events) == 0 {
		return false
	}
	e := &events[len(events)-1]
	if e.Response == nil || e.IsPartial || len(e.Response.Choices) == 0 {
		return false
	}
	m := e.Response.Choices[0].Message
	return m.Role == model.RoleAssistant && strings.TrimSpace(m.Content) != "" && len(m.ToolCalls) == 0
}

// SummaryRunnerOptions 摘要运行参数（阈值任一达标即触发；队列满则丢任务）。
type SummaryRunnerOptions struct {
	EventThreshold int // 距上次摘要的新 scope 事件数阈值
	TokenThreshold int // 距上次摘要的估算 token 阈值
	QueueSize      int // 任务队列长度
	// CascadeFullSession 跨分支级联：事件分散在多个 scope 时，除分支摘要外
	// 再生成一次全会话摘要（多一次 LLM 调用，默认关闭）。
	// 单 scope 会话的"分支摘要即全文摘要"拷贝优化无条件生效，不消耗额外调用。
	CascadeFullSession bool
	// SettleAttempts 落盘稳定等待的最大加载次数（run 进行中入队时等最终回答
	// 落库，防摘要漏轮尾）。0 = 默认 3；1 = 不等待（测试用）。
	SettleAttempts int
}

// summaryJob 一个待处理的摘要任务（worker 消费）。
// ctx 为入队方（SDK runner）携带的请求作用域上下文，含 summaryview 视图、
// cache-safe fork 请求等资产；worker 侧经 WithoutCancel 剥离取消后沿用。
type summaryJob struct {
	ctx       context.Context
	sess      *session.Session
	filterKey string
	force     bool
}

// summaryRunner 摘要异步运行时：有界去重队列 + 单 worker。
type summaryRunner struct {
	summarizer         summary.SessionSummarizer
	eventThreshold     int
	tokenThreshold     int
	cascadeFullSession bool
	settleAttempts     int
	jobs               chan summaryJob
	pending            map[string]struct{}
	pendingMu          sync.Mutex
	done               chan struct{}
	closeOnce          sync.Once
	wg                 sync.WaitGroup
}

// summaryTokenCounter 阈值判定的 token 估算器（与 SDK Checker 同款公开实现）。
var summaryTokenCounter model.TokenCounter = model.NewSimpleTokenCounter()

// SetSummarizer 注入摘要器并启动异步 worker；未注入 = 功能关闭（三方法保持空行为）。
// 重复注入时停掉旧 worker（设置热切换场景预留）。
func (s *SessionService) SetSummarizer(summarizer summary.SessionSummarizer, opts SummaryRunnerOptions) {
	if summarizer == nil {
		return
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 8
	}
	r := &summaryRunner{
		summarizer:         summarizer,
		eventThreshold:     opts.EventThreshold,
		tokenThreshold:     opts.TokenThreshold,
		cascadeFullSession: opts.CascadeFullSession,
		settleAttempts:     opts.SettleAttempts,
		jobs:               make(chan summaryJob, opts.QueueSize),
		pending:            make(map[string]struct{}),
		done:               make(chan struct{}),
	}
	if r.settleAttempts <= 0 {
		r.settleAttempts = summarySettleAttempts
	}
	r.wg.Add(1)
	go s.loopSummary(r)

	s.sumMu.Lock()
	prev := s.sum
	s.sum = r
	s.sumMu.Unlock()
	if prev != nil {
		prev.close()
	}
}

// stopSummarizer 停止摘要 worker（Close 时调用；幂等）。
func (s *SessionService) stopSummarizer() {
	s.sumMu.Lock()
	r := s.sum
	s.sum = nil
	s.sumMu.Unlock()
	if r != nil {
		r.close()
	}
}

// sumRunner 返回当前摘要运行时（未注入返回 nil）。
func (s *SessionService) sumRunner() *summaryRunner {
	s.sumMu.Lock()
	defer s.sumMu.Unlock()
	return s.sum
}

// close 停止 worker（等待在途任务收尾；幂等）。
func (r *summaryRunner) close() {
	r.closeOnce.Do(func() { close(r.done) })
	r.wg.Wait()
}

// loopSummary worker 主循环：取任务 → 处理 → 释放去重键。
// 去重键在处理期间保持占用：处理中的新触发被丢弃，由下一轮触发自愈（至多落后一轮）。
func (s *SessionService) loopSummary(r *summaryRunner) {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			return
		case job := <-r.jobs:
			key := job.sess.ID + "|" + job.filterKey
			s.processSummaryJob(r, job)
			r.pendingMu.Lock()
			delete(r.pending, key)
			r.pendingMu.Unlock()
		}
	}
}

// processSummaryJob 用独立超时上下文处理单个任务（与请求侧 ctx 解耦）。
// runner 由调用方传入（队列归属者），不回读全局 s.sum——Close 置 nil 窗口内，
// 在途任务仍可用所属 runner 安全收尾，避免 nil 解引用。
//
// 鲁棒性设计：
//  1. panic 恢复：worker goroutine 内未预期 panic 只记录日志，不杀进程；
//  2. 重新加载最新会话 + 落盘稳定等待：入队发生在最终事件落库前，job 携带的
//     内存会话可能缺轮尾事件，处理前从存储重读，并等会话以最终 assistant 文本
//     收尾（有重试上限）；读取失败降级用 job 对象；
//  3. ctx 穿线：WithoutCancel 剥离入队方取消信号（摘要在请求结束后仍继续），
//     CloneContext 保留请求作用域值（summaryview/fork/report 等到达 SDK
//     Summarize 层）；job.ctx 为空（测试直调）时回退 Background。
func (s *SessionService) processSummaryJob(r *summaryRunner, job summaryJob) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Errorf("[Absorb] panic in summary worker recovered (session=%s filterKey=%q): %v",
				job.sess.ID, job.filterKey, rec)
		}
	}()
	base := job.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(agent.CloneContext(base)), summaryJobTimeout)
	defer cancel()

	sess := job.sess
	key := session.Key{AppName: job.sess.AppName, UserID: job.sess.UserID, SessionID: job.sess.ID}
	for attempt := 0; attempt < r.settleAttempts; attempt++ {
		latest, err := s.store.GetSession(ctx, key)
		if err != nil {
			log.Warnf("[Absorb] summary job reload session failed (session=%s, attempt=%d): %v",
				job.sess.ID, attempt, err)
			break
		}
		if latest == nil {
			break
		}
		sess = latest
		if endsWithFinalAssistant(latest.Events) {
			break // 落盘稳定：已看到完整轮次
		}
		if attempt == r.settleAttempts-1 {
			break // 重试上限：按现状处理（被取消的悬挂轮等不到最终文本）
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(summarySettleDelay):
		}
	}

	sum, err := s.createSummary(ctx, r, sess, job.filterKey, job.force)
	if err != nil {
		log.Warnf("[Absorb] session summary failed (session=%s filterKey=%q): %v",
			job.sess.ID, job.filterKey, err)
		return
	}
	// 重新加载后计算源与 runner 持有的不是同一对象：把新摘要镜像回原对象，
	// 同轮后续消费与增量判定立即可见（DB 为权威来源）。
	if sum != nil && sess != job.sess {
		mirrorSummary(job.sess, job.filterKey, sum)
	}
}

// EnqueueSummaryJob 入队一个摘要任务（实现 session.Service 契约）。
// ctx 携带 runner 放入的请求作用域资产（summaryview/fork/report），经 WithoutCancel
// 剥离取消后传递给 worker——摘要在请求结束后仍需继续，但请求值必须保留。
// 去重 key = 会话ID|filterKey；队列满则丢弃并告警（摘要允许有损，不反压问答主链路）。
func (s *SessionService) EnqueueSummaryJob(ctx context.Context, sess *session.Session, filterKey string, force bool) error {
	if sess == nil {
		return session.ErrNilSession
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r := s.sumRunner()
	if r == nil {
		return nil
	}
	key := sess.ID + "|" + filterKey
	r.pendingMu.Lock()
	if _, dup := r.pending[key]; dup {
		r.pendingMu.Unlock()
		return nil
	}
	select {
	case r.jobs <- summaryJob{ctx: ctx, sess: sess, filterKey: filterKey, force: force}:
		r.pending[key] = struct{}{}
		r.pendingMu.Unlock()
		return nil
	default:
		r.pendingMu.Unlock()
		log.Warnf("[Absorb] summary queue full, job dropped (session=%s filterKey=%q)", sess.ID, filterKey)
		return nil
	}
}

// CreateSessionSummary 生成并持久化会话摘要（实现 session.Service 契约；
// 异步路径由 worker 调用，亦可同步直调）。返回生成的摘要（无增量/未触发时为 nil）。
func (s *SessionService) CreateSessionSummary(ctx context.Context, sess *session.Session, filterKey string, force bool) error {
	if sess == nil {
		return session.ErrNilSession
	}
	r := s.sumRunner()
	if r == nil {
		return nil
	}
	_, err := s.createSummary(ctx, r, sess, filterKey, force)
	return err
}

// createSummary 摘要主流程：增量判定 → scope 过滤 → 滚动累积 → LLM 摘要 → 持久化。
// 返回本次生成的摘要指针（无新增/未触发阈值/空文本时为 nil）。
func (s *SessionService) createSummary(ctx context.Context, r *summaryRunner, sess *session.Session, filterKey string, force bool) (*session.Summary, error) {
	// 增量判定：距该 scope 上次摘要边界的新增 scope 事件是否达到任一阈值。
	// 边界读 Summaries[filterKey].Boundary（per-scope 持久化，与 recall cutoffs 同源），
	// 不读会话级 state 键——多 scope 交错（跨 tab）时共享游标会把早于他 scope
	// cutoff 的未摘要事件永久跳过，边界乱序回退也会引发重复摘要。
	since, lastBoundaryID := scopeBoundary(sess, filterKey)
	delta := scopedEvents(sess, filterKey, since, lastBoundaryID)
	if len(delta) == 0 {
		return nil, nil
	}
	if !force && !r.thresholdFired(ctx, delta) {
		return nil, nil
	}

	start := time.Now()
	// scope 会话：克隆并替换为过滤后事件（Summarize 把新 boundary 写入克隆的 State，
	// 不污染 runner 持有的原会话）。清掉克隆自会话的共享边界 state 键：当前 SDK 的
	// Summarize 不做增量过滤，防未来版本按 stale 键对本轮 delta 二次过滤。
	scopeSess := sess.Clone()
	// 滚动累积：旧摘要按"合成系统事件"规格插到增量头部，经 {conversation_text}
	// 进入提示词，模板第 5 条指导模型合并增补而非覆盖。不做累积 = 每次只摘要增量
	// 并整体替换，早期内容会随"摘要被换 + 原文被裁"永久丢失。
	prev := previousSummaryText(sess, filterKey)
	scopeSess.Events = prependPrevSummary(prev, delta, time.Now())
	scopeSess.DeleteState(session.SummaryLastIncludedTimestampStateKey)
	scopeSess.DeleteState(session.SummaryLastIncludedEventIDStateKey)
	text, err := r.summarizer.Summarize(ctx, scopeSess)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	// 回溯提示不持久化：Summaries 存纯摘要，hint 由消费侧拼接（chat 包
	// summaryCardFormatter）。fork 模式下父请求里的摘要卡会进入摘要输入，模型输出
	// 可能复制 hint 行（含旧版本持久化带 hint 的摘要经滚动合成头进入输入的情形），
	// 持久化前统一剥离，避免滚动累积。
	text = stripRecallHint(strings.TrimRight(text, "\n"))

	// boundary：优先用 summarizer 记录的 last_included（事件级精确），
	// 回退最后一个 scope 事件的时间与 ID。注意用 delta 而非 scopeSess.Events
	// 取回退值——后者头部多了合成摘要事件，末条虽相同但语义上 delta 才是源。
	cutoff := readLastIncludedTime(scopeSess)
	lastEventID := ""
	if raw, ok := scopeSess.GetState(session.SummaryLastIncludedEventIDStateKey); ok {
		lastEventID = string(raw)
	}
	if cutoff.IsZero() {
		last := delta[len(delta)-1]
		cutoff = last.Timestamp
		lastEventID = last.ID
	}

	sum := &session.Summary{
		Summary:   text,
		UpdatedAt: cutoff,
		Boundary:  session.NewSummaryBoundaryWithEventID(filterKey, cutoff, lastEventID),
	}

	// 写回内存会话（同轮后续消费/增量判定立即可见）+ 持久化。
	// 边界只落在 Summaries[filterKey].Boundary（per-scope），不写会话级 state 键。
	// 级联判定放在锁外：allEventsInScope 要取 EventMu，避免 SummariesMu→EventMu 嵌套锁。
	singleScope := filterKey != "" && allEventsInScope(sess, filterKey)
	needCascade := filterKey != "" && !singleScope && r.cascadeFullSession

	sess.SummariesMu.Lock()
	if sess.Summaries == nil {
		sess.Summaries = make(map[string]*session.Summary)
	}
	sess.Summaries[filterKey] = sum
	// 级联（单 scope 零成本优化）：全部事件都在本分支内时，分支摘要即全会话摘要，
	// 拷贝到全文 key，免第二次 LLM 调用。跨分支级联受配置门控，在锁外另起一次
	// createSummary（自带持久化），避免重入死锁。
	if singleScope {
		full := sum.Clone()
		full.Boundary = session.NewSummaryBoundaryWithEventID(
			session.SummaryFilterKeyAllContents, cutoff, lastEventID)
		sess.Summaries[session.SummaryFilterKeyAllContents] = full
	}
	snapshot := make(map[string]*session.Summary, len(sess.Summaries))
	for k, v := range sess.Summaries {
		snapshot[k] = v.Clone()
	}
	sess.SummariesMu.Unlock()
	key := session.Key{AppName: sess.AppName, UserID: sess.UserID, SessionID: sess.ID}
	if err := s.store.SaveSummaries(ctx, key, snapshot, nil); err != nil {
		return nil, err
	}
	if needCascade {
		if _, err := s.createSummary(ctx, r, sess, session.SummaryFilterKeyAllContents, force); err != nil {
			log.Warnf("[Absorb] cascade full-session summary failed (session=%s): %v", sess.ID, err)
		}
	}

	// 日志富化：标识还原为人类可读网址（映射表查询失败不影响主流程）
	logURL := ""
	if u, uerr := s.store.ScopeURL(ctx, key.AppName, key.UserID, key.SessionID, filterKey); uerr == nil {
		logURL = u
	}
	log.Infof("[Absorb] session summary generated (session=%s filterKey=%q url=%q events=%d chars=%d rolling=%t cascade=%t elapsed=%s)",
		sess.ID, filterKey, logURL, len(delta), len(text), prev != "", singleScope,
		time.Since(start).Round(time.Millisecond))
	return sum, nil
}

// stripRecallHint 剥离文本尾部的回溯提示行（createSummary 持久化前兜底：
// 模型输出可能复制输入中的 hint；带 hint 的旧摘要经滚动合成头进入输入同理）。
// hint 字面量为 chat↔store 跨包契约，单一事实源 kind.SummaryRecallHint。
func stripRecallHint(text string) string {
	t := strings.TrimRight(strings.TrimSpace(text), "\n")
	t = strings.TrimSuffix(t, kind.SummaryRecallHint)
	return strings.TrimRight(t, "\n")
}

// previousSummaryText 读取该 scope 上一次持久化的摘要文本（standalone 回退路径
// 滚动累积的输入）。Summaries 存纯摘要（无 hint），直接返回；无摘要返回空串。
func previousSummaryText(sess *session.Session, filterKey string) string {
	if sess == nil {
		return ""
	}
	sess.SummariesMu.RLock()
	defer sess.SummariesMu.RUnlock()
	sum := sess.Summaries[filterKey]
	if sum == nil {
		return ""
	}
	return sum.Summary
}

// prependPrevSummary 把旧摘要作为合成系统事件插到增量事件头部（滚动累积）。
// Author=system、Timestamp=now（不早于首条增量事件），满足公开层
// hasPrependedSummaryContext 与 removePreviousSummaryEvent 的识别形状；
// 旧摘要随 {conversation_text} 进入提示词。
func prependPrevSummary(prevSummary string, delta []event.Event, now time.Time) []event.Event {
	if prevSummary == "" {
		return delta
	}
	out := make([]event.Event, 0, len(delta)+1)
	out = append(out, event.Event{
		Author: "system",
		Response: &model.Response{Choices: []model.Choice{{
			Message: model.NewSystemMessage(prevSummary),
		}}},
		Timestamp: now,
	})
	return append(out, delta...)
}

// allEventsInScope 全会话事件是否都在 filterKey 分支内（Filter 双向前缀匹配）。
// 单 scope 会话的分支摘要与全会话摘要等价，可拷贝免二次调用。
func allEventsInScope(sess *session.Session, filterKey string) bool {
	if sess == nil || filterKey == "" {
		return false
	}
	sess.EventMu.RLock()
	defer sess.EventMu.RUnlock()
	if len(sess.Events) == 0 {
		return false
	}
	for i := range sess.Events {
		if !sess.Events[i].Filter(filterKey) {
			return false
		}
	}
	return true
}

// mirrorSummary 把新摘要镜像写回 runner 持有的内存会话对象
// （重新加载后计算源与 job 对象不是同一实例，保持同轮消费立即可见）。
func mirrorSummary(dst *session.Session, filterKey string, sum *session.Summary) {
	if dst == nil || sum == nil {
		return
	}
	dst.SummariesMu.Lock()
	defer dst.SummariesMu.Unlock()
	if dst.Summaries == nil {
		dst.Summaries = make(map[string]*session.Summary)
	}
	dst.Summaries[filterKey] = sum.Clone()
}

// GetSessionSummaryText 按 filterKey 读取已持久化的摘要文本（实现 session.Service 契约）。
// 不查库：消费方传入的 sess 由 GetSession 构建，Summaries 已反序列化。
// 返回文本尾部拼接回溯提示（持久化为纯摘要，hint 仅在消费侧存在——与 chat 包
// summaryCardFormatter 的卡片注入保持一致）。无 opts = 全会话摘要
// （SummaryFilterKeyAllContents）；无摘要返回 ("", false)。
func (s *SessionService) GetSessionSummaryText(_ context.Context, sess *session.Session, opts ...session.SummaryOption) (string, bool) {
	if sess == nil {
		return "", false
	}
	var o session.SummaryOptions
	for _, opt := range opts {
		opt(&o)
	}
	sess.SummariesMu.RLock()
	sum := sess.Summaries[o.FilterKey]
	sess.SummariesMu.RUnlock()
	if sum == nil || strings.TrimSpace(sum.Summary) == "" {
		return "", false
	}
	return sum.Summary + kind.SummaryRecallHint, true
}

// thresholdFired 任一阈值达标即触发（事件数 or 估算 token）。
func (r *summaryRunner) thresholdFired(ctx context.Context, delta []event.Event) bool {
	if r.eventThreshold > 0 && len(delta) > r.eventThreshold {
		return true
	}
	if r.tokenThreshold > 0 && estimateTokens(ctx, delta) > r.tokenThreshold {
		return true
	}
	return false
}

// scopedEvents 过滤出 filterKey scope 内、上次摘要边界之后的候选事件。
// lastEventID 非空时按事件序严格取其后（边界事件自身与其之前的不再进入增量，
// 时间戳相同也不会重复摘要）；边界事件不在当前列表（如被裁剪）时回退 since 时间
// 过滤（宁重勿丢）。filterKey=="" 表示全会话 scope；合成/遗留的无标事件
// （FilterKey==""）一并纳入，与 SDK 分支 scope 语义一致。
func scopedEvents(sess *session.Session, filterKey string, since time.Time, lastEventID string) []event.Event {
	sess.EventMu.RLock()
	defer sess.EventMu.RUnlock()
	scope := make([]event.Event, 0, len(sess.Events))
	for i := range sess.Events {
		e := sess.Events[i]
		if filterKey != "" && e.FilterKey != filterKey && e.FilterKey != "" {
			continue
		}
		scope = append(scope, e)
	}
	if lastEventID != "" {
		for i := range scope {
			if scope[i].ID == lastEventID {
				return append([]event.Event(nil), scope[i+1:]...)
			}
		}
	}
	out := make([]event.Event, 0, len(scope))
	for i := range scope {
		if !since.IsZero() && scope[i].Timestamp.Before(since) {
			continue
		}
		out = append(out, scope[i])
	}
	return out
}

// scopeBoundary 读取该 scope 上次摘要的增量边界（Summaries[filterKey].Boundary，
// 随摘要 per-scope 持久化，与 recall cutoffs 同源）。返回 cutoff 时间与边界事件 ID；
// 无摘要/无边界返回零值/空串（= 全量，降级安全）。
func scopeBoundary(sess *session.Session, filterKey string) (time.Time, string) {
	sess.SummariesMu.RLock()
	defer sess.SummariesMu.RUnlock()
	sum := sess.Summaries[filterKey]
	if sum == nil || sum.Boundary == nil {
		return time.Time{}, ""
	}
	return sum.Boundary.CutoffAt, sum.Boundary.LastEventID
}

// readLastIncludedTime 从会话 state 读取上次摘要覆盖到的事件时间（RFC3339Nano）。
// 无记录/格式坏返回零值（= 全量，与 SDK filterDeltaEvents 的降级语义一致）。
func readLastIncludedTime(sess *session.Session) time.Time {
	if sess == nil {
		return time.Time{}
	}
	raw, ok := sess.GetState(session.SummaryLastIncludedTimestampStateKey)
	if !ok || len(raw) == 0 {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, string(raw))
	if err != nil {
		log.Warnf("[Absorb] invalid %s in session state (session=%s): %v",
			session.SummaryLastIncludedTimestampStateKey, sess.ID, err)
		return time.Time{}
	}
	return ts
}

// estimateTokens 粗估 delta 事件的 token 数（SimpleTokenCounter；失败回退字节/2 的中英混合经验值）。
func estimateTokens(ctx context.Context, events []event.Event) int {
	var b strings.Builder
	for i := range events {
		e := events[i]
		if e.Response == nil {
			continue
		}
		for _, ch := range e.Response.Choices {
			b.WriteString(ch.Message.Content)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return 0
	}
	tokens, err := summaryTokenCounter.CountTokens(ctx, model.NewUserMessage(b.String()))
	if err != nil {
		return b.Len() / 2
	}
	return tokens
}
