package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/log"
	"absorb-agent/internal/store/storage"
)

// recall_history 召回组装：摘要折叠后细节的按需回溯。
// 职责：把 store 层原始查询组装成模型可读文本——index 目录（seq|作者|时间|覆盖|预览）
// 与 fetch 原文切片（预算内），供 tool/recall_history 直接返回给 LLM。

const (
	// recallFetchMaxEvents 单次 fetch 返回的事件数上限（防灌爆上下文）。
	recallFetchMaxEvents = 12
	// recallFetchMaxTokens 单次 fetch 返回的估算 token 上限（SimpleTokenCounter
	// 4 rune ≈ 1 token，约 16000 rune 内容）。
	recallFetchMaxTokens = 4000
	// recallFetchSingleMaxRunes 超预算单条事件的 rune 截断长度（首条保证可见）。
	recallFetchSingleMaxRunes = 2000
	// recallIndexPreviewRunes 目录行预览长度（rune）。
	recallIndexPreviewRunes = 60
)

// recallEmptyText 空会话/空 scope 的统一空态文案。
const recallEmptyText = "本会话暂无可回溯历史。"

// ListEventIndex 查询某 scope 的事件目录（透传 store 层）。
func (s *SessionService) ListEventIndex(ctx context.Context, key session.Key, filterKey string) ([]storage.EventIndexEntry, error) {
	return s.store.ListEventIndex(ctx, key, filterKey)
}

// FetchEventSlice 按 seq 闭区间查询 scope 事件（透传 store 层）。
func (s *SessionService) FetchEventSlice(ctx context.Context, key session.Key, filterKey string, fromSeq, toSeq int64) ([]storage.RecallEvent, error) {
	return s.store.FetchEventSlice(ctx, key, filterKey, fromSeq, toSeq)
}

// SearchEvents 按 keywords 检索 scope 事件（透传 store 层）。
func (s *SessionService) SearchEvents(ctx context.Context, key session.Key, filterKey string, keywords []string, limit int) ([]storage.RecallEvent, error) {
	return s.store.SearchEvents(ctx, key, filterKey, keywords, limit)
}

// SummaryCutoffs 返回各 scope 摘要 boundary 的 CutoffAt（index 目录“摘要已覆盖”
// 列的判定依据）。会话不存在/无摘要/读失败返回空 map（covered 全 false，降级安全）。
func (s *SessionService) SummaryCutoffs(ctx context.Context, key session.Key) map[string]time.Time {
	sums, err := s.store.LoadSummaries(ctx, key)
	if err != nil {
		log.Warnf("[Absorb] recall load summaries failed (session=%s): %v", key.SessionID, err)
		return nil
	}
	out := make(map[string]time.Time, len(sums))
	for k, v := range sums {
		if v == nil || v.Boundary == nil {
			continue
		}
		out[k] = v.Boundary.CutoffAt
	}
	return out
}

// RecallIndex 组装 scope 的对话轮次目录文本（每行 seq | 作者 | 时间 | 摘要覆盖 | 预览）。
// cutoff 为该 scope 摘要 boundary 的 CutoffAt（零值 = 无摘要，全部未覆盖）；
// keywords 非空时仅保留预览中含任一关键词的行（宽筛，precise 检索走 fetch）。
func (s *SessionService) RecallIndex(ctx context.Context, key session.Key, filterKey string, keywords []string, cutoff time.Time) (string, error) {
	entries, err := s.store.ListEventIndex(ctx, key, filterKey)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return recallEmptyText, nil
	}
	kws := normalizeKeywords(keywords)
	var b strings.Builder
	shown := 0
	for _, e := range entries {
		if strings.TrimSpace(e.Preview) == "" {
			continue // 纯工具调用等无文本事件不进目录（含仅空白字符的前导事件）
		}
		if len(kws) > 0 && !containsAnyLower(e.Preview, kws) {
			continue
		}
		shown++
		covered := "未覆盖"
		if !cutoff.IsZero() && !e.Time.After(cutoff) {
			covered = "已覆盖"
		}
		fmt.Fprintf(&b, "#%d | %s | %s | %s | %s\n",
			e.Seq, e.Author, e.Time.Local().Format("2006-01-02 15:04"), covered,
			truncateRunes(e.Preview, recallIndexPreviewRunes))
	}
	if shown == 0 {
		return "目录中没有预览命中关键词的行；请换关键词，或去掉 keywords 查看全部目录。", nil
	}
	log.Infof("[Absorb] recall index (session=%s scope=%q entries=%d shown=%d)",
		key.SessionID, filterKey, len(entries), shown)
	return strings.TrimRight(b.String(), "\n"), nil
}

// RecallFetch 组装 fetch 原文切片：keywords 优先定向检索，无 keywords 走 seq 闭区间；
// 单次预算 ≤12 条事件且 ≤4000 token，超限截断并附“缩小 keywords 或 seq_range”提示。
func (s *SessionService) RecallFetch(ctx context.Context, key session.Key, filterKey string, keywords []string, seqRange []int64) (string, error) {
	var events []storage.RecallEvent
	var err error
	kws := normalizeKeywords(keywords)
	switch {
	case len(kws) > 0:
		events, err = s.store.SearchEvents(ctx, key, filterKey, keywords, recallFetchMaxEvents)
		if err == nil && len(events) == 0 {
			return fmt.Sprintf("keywords %v 在该 scope 的历史原文中零命中；请换用原文用词（可先 action=index 查看目录）。", keywords), nil
		}
	case len(seqRange) == 2:
		events, err = s.store.FetchEventSlice(ctx, key, filterKey, seqRange[0], seqRange[1])
	default:
		return "fetch 需要 keywords 或 seq_range 之一（seq_range 形如 [fromSeq, toSeq]）。", nil
	}
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return recallEmptyText, nil
	}

	// 预算截断：条数与估算 token 双上限。首条保证可见（单条超预算时 rune 截断），
	// 后续事件累计超任一上限即停止。
	kept := make([]storage.RecallEvent, 0, len(events))
	tokens := 0
	overBudget := false
	for _, e := range events {
		if len(kept) == recallFetchMaxEvents {
			overBudget = true
			break
		}
		content := e.Content
		if len([]rune(content)) > recallFetchSingleMaxRunes {
			content = truncateRunes(content, recallFetchSingleMaxRunes) + "…[已截断]"
		}
		add := estimateRecallTokens(ctx, content)
		if len(kept) > 0 && tokens+add > recallFetchMaxTokens {
			overBudget = true
			break
		}
		tokens += add
		kept = append(kept, storage.RecallEvent{Seq: e.Seq, Author: e.Author, Time: e.Time, Content: content})
	}

	var b strings.Builder
	for _, e := range kept {
		fmt.Fprintf(&b, "#%d | %s | %s\n%s\n\n",
			e.Seq, e.Author, e.Time.Local().Format("2006-01-02 15:04"), e.Content)
	}
	if overBudget {
		fmt.Fprintf(&b, "（已按预算截断：命中 %d 条，仅返回前 %d 条；请缩小 keywords 或缩小 seq_range 获取其余内容）\n", len(events), len(kept))
	}
	log.Infof("[Absorb] recall fetch (session=%s scope=%q hits=%d returned=%d estTokens=%d)",
		key.SessionID, filterKey, len(events), len(kept), tokens)
	return strings.TrimRight(b.String(), "\n"), nil
}

// estimateRecallTokens 单条文本粗估 token（SimpleTokenCounter，4 rune ≈ 1 token；
// 失败回退字节/2 的中英混合经验值，与 estimateTokens 同策略）。
func estimateRecallTokens(ctx context.Context, text string) int {
	if text == "" {
		return 0
	}
	tokens, err := summaryTokenCounter.CountTokens(ctx, model.NewUserMessage(text))
	if err != nil {
		return len(text) / 2
	}
	return tokens
}

// normalizeKeywords 关键词清洗：trim、小写、去空、去重（匹配统一小写比较）。
func normalizeKeywords(keywords []string) []string {
	out := make([]string, 0, len(keywords))
	seen := make(map[string]struct{}, len(keywords))
	for _, k := range keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// truncateRunes rune 安全截断。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// containsAnyLower 小写包含匹配：s 的小写形态含任一已清洗关键词即命中。
func containsAnyLower(s string, loweredKeywords []string) bool {
	ls := strings.ToLower(s)
	for _, k := range loweredKeywords {
		if strings.Contains(ls, k) {
			return true
		}
	}
	return false
}
