package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/model"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/log"
)

// ---------- L3 用户画像蒸馏 ----------
// L1 原子记忆（memory.db，框架提取器维护）之上周期蒸馏出稳定画像文件（profile.md）。
// 画像走文件式：条目少、全量注入（BuildUserMessage 顶部 <user_profile> 块）。
// 蒸馏为后台 goroutine：启动补跑一次，此后每 interval 按水位（新增原子记忆条数）
// 达标才触发，进程退出即停（原子写中断安全）。

// distillSystemPrompt 蒸馏器系统提示词：增量三分法、证据强度冲突处理、稳定优先、禁止罗列原始条目。
// 冲突规则与增量对照源自 nuwa-skill 蒸馏框架的启发（A/B 实验验证：相似度 0.48→0.93）。
const distillSystemPrompt = `你是一个用户画像蒸馏器。输入为「现有画像」与「长期记忆库中的原子事实」，输出一份蒸馏后的稳定用户画像（Markdown）。
要求：
- 只保留对长期交互有指导意义的稳定信息：身份/技术背景、交互偏好、兴趣领域、学习轨迹概要。
- 增量对照：先把 known_facts 与现有画像对照分类——「推翻」（改写对应条目）、「加深」（充实既有条目细节）、「新增」（补充新条目）；已被画像覆盖且无变化的事实直接忽略，不要复述。
- 信息冲突按证据强度处理，不以新旧为准：兴趣偏好类的新方向若只在零星事实中出现，视为临时兴趣，不推翻画像主体（至多忽略或一句带过）；只有在新方向于多条、不同时期的事实中反复出现，才认定真实迁移——主写新方向，旧方向压缩为一句演化轨迹（「已从 X 转向 Y」）。身份/职业类事实单条即可推翻画像。不强行调和、不并列两个冲突立场。
- 现有画像中仍然准确的条目，原样保留其措辞，不要改写；仅更新确已变化的内容。
- 合并与去重：相似事实合并为一条概括；临时性、一次性、与画像无关的内容丢弃。
- 提炼而非罗列：不要逐条复述 facts；严禁编造 facts 中不存在的信息。
- 篇幅上限 3500 字，在保留稳定条目细节的前提下尽量精炼；直接输出画像正文（Markdown），不要任何额外解释或前言。`

// profileState 蒸馏水位（上次蒸馏时的原子记忆条数），与画像文件同目录存放。
type profileState struct {
	LastCount int       `json:"last_count"`
	UpdatedAt time.Time `json:"updated_at"`
}

func profileStatePath(profilePath string) string {
	return strings.TrimSuffix(profilePath, filepath.Ext(profilePath)) + ".state.json"
}

func loadProfileState(path string) profileState {
	var st profileState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func saveProfileState(path string, st profileState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// shouldDistillProfile 蒸馏水位判定：
//   - 已有画像：新增原子记忆 ≥ minNew 才蒸馏；
//   - 无画像（首次）：只要 count > 0 即蒸馏。
func shouldDistillProfile(count, lastCount int, hasProfile bool, minNew int) bool {
	if count <= 0 {
		return false
	}
	if !hasProfile {
		return true
	}
	return count-lastCount >= minNew
}

// writeProfileAtomic 原子写画像：tmp → remove 旧文件 → rename（Windows rename 不能覆盖）。
func writeProfileAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写临时文件: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("移除旧画像: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换画像: %w", err)
	}
	return nil
}

// profileTracer 蒸馏链路观测（telemetry 关闭时全局为 noop tracer，零开销）。
var profileTracer = otel.Tracer("absorb.profile")

// maxProfileOutputRunes 蒸馏输出篇幅兜底（与 prompt 上限 3500 字、注入截断 4000 runes
// 对齐）：超限截到最后一个段落边界，避免 maxTokens 截断的半句话外溢进画像文件。
const maxProfileOutputRunes = 3500

// clampProfileOutput 超长画像输出收敛到段落边界。
func clampProfileOutput(content string) string {
	runes := []rune(content)
	if len(runes) <= maxProfileOutputRunes {
		return content
	}
	clamped := string(runes[:maxProfileOutputRunes])
	if i := strings.LastIndex(clamped, "\n"); i > 0 {
		clamped = clamped[:i]
	}
	log.Warnf("[Absorb] profile output exceeded %d runes, clamped (was %d)", maxProfileOutputRunes, len(runes))
	return strings.TrimSpace(clamped)
}

// profileLines 统计画像有效行数（非空行），缩水守卫的比较基准。
func profileLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// distillProfile 执行一次画像蒸馏：读 L1 原子记忆 → GLM 蒸馏 → 原子写 profile.md → 推进水位。
// 结果经 span 上报（不含画像正文，只记结构化指标）：result ∈ distilled / skipped_watermark / error。
func distillProfile(ctx context.Context, memSvc memory.Service, uk memory.UserKey, pc conf.ProfileConfig, m model.Model) error {
	ctx, span := profileTracer.Start(ctx, "profile.distill")
	defer span.End()
	fail := func(err error) error {
		span.SetAttributes(attribute.String("profile.result", "error"))
		span.RecordError(err)
		return err
	}
	statePath := profileStatePath(pc.FilePath)
	hasProfile := false
	if _, err := os.Stat(pc.FilePath); err == nil {
		hasProfile = true
	}
	entries, err := memSvc.ReadMemories(ctx, uk, 1000)
	if err != nil {
		return fail(fmt.Errorf("读取原子记忆: %w", err))
	}
	lastCount := loadProfileState(statePath).LastCount
	span.SetAttributes(
		attribute.Int("profile.entries_total", len(entries)),
		attribute.Int("profile.last_count", lastCount),
		attribute.Bool("profile.has_profile", hasProfile),
	)
	if !shouldDistillProfile(len(entries), lastCount, hasProfile, pc.MinNewEntries) {
		log.Debugf("[Absorb] profile distill skipped: watermark not met (total %d, last %d)", len(entries), lastCount)
		span.SetAttributes(attribute.String("profile.result", "skipped_watermark"))
		return nil // 水位未达标，跳过本轮
	}

	// 新→旧排序，取最近 400 条，约束蒸馏输入规模。水位必须记录截断前的真实总数：
	// 若记截断后条数，存量超过 400 后 count-lastCount 恒达标，每 tick 都会重复蒸馏。
	total := len(entries)
	slices.SortFunc(entries, func(a, b *memory.Entry) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	if len(entries) > 400 {
		entries = entries[:400]
	}
	var facts strings.Builder
	for i, e := range entries {
		facts.WriteString(fmt.Sprintf("%d. %s（topics: %s）\n", i+1, e.Memory.Memory, strings.Join(e.Memory.Topics, "、")))
	}
	span.SetAttributes(
		attribute.Int("profile.entries_used", len(entries)),
		attribute.Int("profile.facts_chars", facts.Len()),
	)
	existing, _ := os.ReadFile(pc.FilePath) // 不存在则为空，作为首蒸基线

	userMsg := fmt.Sprintf("<existing_profile>\n%s\n</existing_profile>\n\n<known_facts>\n%s</known_facts>\n\n请输出蒸馏后的用户画像。", string(existing), facts.String())
	content, err := generateText(ctx, m, distillSystemPrompt, userMsg)
	if err != nil {
		return fail(fmt.Errorf("蒸馏调用: %w", err))
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fail(fmt.Errorf("蒸馏结果为空"))
	}
	content = clampProfileOutput(content)
	// 缩水守卫（棘轮）：新版有效行数不足旧版一半视为异常蒸馏（模型抽风蒸没了稳定
	// 条目），保留旧画像、水位不推进，下轮自然重试。首蒸旧文件为空（0 行）不触发。
	if oldLines := profileLines(string(existing)); oldLines > 0 && profileLines(content)*2 < oldLines {
		return fail(fmt.Errorf("蒸馏结果疑似缩水: %d 行 -> %d 行，保留旧画像", oldLines, profileLines(content)))
	}
	span.SetAttributes(attribute.Int("profile.output_runes", len([]rune(content))))
	if err := writeProfileAtomic(pc.FilePath, content); err != nil {
		return fail(err)
	}
	if err := saveProfileState(statePath, profileState{LastCount: total, UpdatedAt: time.Now()}); err != nil {
		log.Warnf("[Absorb] write profile watermark failed: %v", err)
	}
	span.SetAttributes(attribute.String("profile.result", "distilled"))
	log.Infof("[Absorb] profile distilled: %s (from %d atomic memories)", pc.FilePath, len(entries))
	return nil
}

// generateText 单轮非流式文本生成（蒸馏专用：低温度、关思考、限长）。
// maxTokens 5000 为 3500 字上限留出分词余量。
func generateText(ctx context.Context, m model.Model, system, user string) (string, error) {
	disabled := false
	temp := 0.3
	maxTokens := 5000
	req := &model.Request{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: system},
			{Role: model.RoleUser, Content: user},
		},
		GenerationConfig: model.GenerationConfig{
			Stream:          false,
			Temperature:     &temp,
			MaxTokens:       &maxTokens,
			ThinkingEnabled: &disabled,
		},
	}
	respCh, err := m.GenerateContent(ctx, req)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for resp := range respCh {
		if resp == nil {
			continue
		}
		for _, ch := range resp.Choices {
			sb.WriteString(ch.Message.Content)
		}
	}
	return sb.String(), nil
}

// StartProfileDistiller 启动画像蒸馏后台 goroutine：启动即补跑一次水位检查
// （上次退出时水位已达标无需等首个 tick），此后每 interval 检查一次；进程退出即停。
// uk 为记忆归属维度（app + user），与记忆库读写保持同一 UserKey。
// enabledFn 为记忆运行时开关（nil=始终启用）：关闭态跳过蒸馏，下轮再查（热切换）。
func StartProfileDistiller(memSvc memory.Service, cfg conf.Config, m model.Model, uk memory.UserKey, enabledFn func() bool) {
	pc := cfg.Memory.Profile
	interval := time.Duration(pc.IntervalMinutes) * time.Minute
	go func() {
		runOnce := func() {
			if enabledFn != nil && !enabledFn() {
				return // 记忆总开关关闭：跳过本轮，进程常驻
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := distillProfile(ctx, memSvc, uk, pc, m); err != nil {
				log.Warnf("[Absorb] profile distillation failed: %v", err)
			}
		}
		runOnce() // 启动补跑
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			runOnce()
		}
	}()
}
