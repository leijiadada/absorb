package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"absorb-agent/internal/store/storage"
)

// Service 知识持久化业务服务（M1）。在 storage 契约之上补充业务语义：
// dedup_key 计算与幂等创建、入参校验（来源 ≥ 1）、状态枚举、存在性校验。
// 桥接层（M1 第三步）依赖本模块而非直接操作 storage。
type Service struct {
	st storage.Storage
	// ks 关键词生成源（M3.1 检索）：nil 时 SearchRelevant 惰性取规则源；
	// 评测/确定性测试经 SetKeywordSource 注入固定源。
	ks       KeywordSource
	onChange func(KnowledgeEvent) // 变更事件出口（SetOnChange 注册；nil=关闭）
}

func NewService(st storage.Storage) *Service {
	return &Service{st: st}
}

// ErrNotFound 目标知识不存在（Get 命中空、Update/SetStatus 前置校验失败）。
var ErrNotFound = errors.New("knowledge: not found")

// ErrValidation 入参校验失败（必填缺失、枚举非法等）。HTTP 层据此映射 400。
var ErrValidation = errors.New("knowledge: invalid input")

// SourceInput 创建时的单条来源。SourceType 空串按 quote 处理（划词证据）；
// conversation 来源（无划词对话轮次）quote 恒为空，服务层强制置空。
type SourceInput struct {
	SourceType      string
	SourceTitle     string
	SourceURL       string
	QuoteText       string // quote 类型必填；conversation 类型被服务层清空
	SourceSessionID string
	SourceEventID   string
}

// CreateInput 创建入参。Origin* 由桥接层从当前问答上下文透传；
// DedupKey 不在入参中——由服务层从 Origin* 计算，防伪造绕过幂等。
type CreateInput struct {
	Title             string // 必填
	AISummary         string
	UserUnderstanding string
	OriginSessionID   string
	OriginEventID     string
	Sources           []SourceInput // ≥ 1：M1 知识均来自阅读，必须有原始引用
}

// Create 创建知识条目。幂等：同一回答事件（Origin 二元组相同）重复提交
// 返回既有条目与 created=false，不产生重复记录。
// M2.5 增强：L1 来源会话校验（不创建缺少依据的记录）；带 URL 的来源按 URL
// upsert documents 锚点回填 source_document_id；conversation 来源强制清空
// quote（服务端不变量，防客户端旧逻辑/误用把非原文文本写进证据字段）。
func (s *Service) Create(ctx context.Context, app, user string, in CreateInput) (*storage.KnowledgeItem, bool, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return nil, false, fmt.Errorf("%w: title is required", ErrValidation)
	}
	if len(in.Sources) == 0 {
		return nil, false, fmt.Errorf("%w: at least one source is required", ErrValidation)
	}
	if err := s.checkOriginSession(ctx, app, user, in.OriginSessionID); err != nil {
		return nil, false, err
	}
	// 来源 URL → Document 锚点（同 URL 多来源只 upsert 一次）
	docIDs := make(map[string]string)
	for _, src := range in.Sources {
		u := strings.TrimSpace(src.SourceURL)
		if u == "" {
			continue
		}
		if _, ok := docIDs[u]; ok {
			continue
		}
		doc, err := s.st.UpsertDocument(ctx, app, user, u, strings.TrimSpace(src.SourceTitle))
		if err != nil {
			return nil, false, err
		}
		docIDs[u] = doc.ID
	}
	item := &storage.KnowledgeItem{
		ID:                newID(),
		AppName:           app,
		UserID:            user,
		Title:             in.Title,
		AISummary:         in.AISummary,
		UserUnderstanding: in.UserUnderstanding,
		Status:            storage.KnowledgeStatusActive,
		OriginSessionID:   in.OriginSessionID,
		OriginEventID:     in.OriginEventID,
		DedupKey:          dedupKey(in.OriginSessionID, in.OriginEventID),
	}
	sources := make([]*storage.KnowledgeSource, 0, len(in.Sources))
	for i, src := range in.Sources {
		typ := src.SourceType
		if typ == "" {
			typ = storage.KnowledgeSourceQuote
		}
		quote := strings.TrimSpace(src.QuoteText)
		if typ == storage.KnowledgeSourceConversation {
			quote = "" // 服务端不变量：对话来源不携带任何"伪摘录"
		} else if quote == "" {
			return nil, false, fmt.Errorf("%w: source[%d].quote is required", ErrValidation, i)
		}
		sources = append(sources, &storage.KnowledgeSource{
			ID:               newID(),
			ItemID:           item.ID,
			AppName:          app,
			UserID:           user,
			SourceType:       typ,
			SourceTitle:      src.SourceTitle,
			SourceURL:        src.SourceURL,
			QuoteText:        quote,
			SourceSessionID:  src.SourceSessionID,
			SourceEventID:    src.SourceEventID,
			SourceDocumentID: docIDs[strings.TrimSpace(src.SourceURL)],
		})
	}
	saved, created, err := s.st.CreateKnowledge(ctx, app, user, item, sources)
	if err != nil {
		return nil, false, err
	}
	// 事件仅在真正新建时发出；幂等命中（created=false）保持静默
	if created {
		s.emit(KnowledgeEvent{Kind: EventCreated, Item: saved})
	}
	return saved, created, nil
}

// Get 读取条目与来源；不存在返回 ErrNotFound。
func (s *Service) Get(ctx context.Context, app, user, id string) (*storage.KnowledgeItem, []*storage.KnowledgeSource, error) {
	item, sources, err := s.st.GetKnowledge(ctx, app, user, id)
	if err != nil {
		return nil, nil, err
	}
	if item == nil {
		return nil, nil, ErrNotFound
	}
	return item, sources, nil
}

// List 按状态列出（status 空串=全部）。
func (s *Service) List(ctx context.Context, app, user, status string) ([]*storage.KnowledgeItem, error) {
	if status != "" && status != storage.KnowledgeStatusActive && status != storage.KnowledgeStatusArchived {
		return nil, fmt.Errorf("%w: invalid status %q", ErrValidation, status)
	}
	return s.st.ListKnowledge(ctx, app, user, status)
}

// Update 编辑可变文本（title/ai_summary/user_understanding）。
// 只触碰 items 三列，原始引用不受影响（"改提炼不改原始引用"）。
func (s *Service) Update(ctx context.Context, app, user, id, title, aiSummary, userUnderstanding string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("%w: title is required", ErrValidation)
	}
	if _, _, err := s.Get(ctx, app, user, id); err != nil {
		return err
	}
	if err := s.st.UpdateKnowledgeText(ctx, app, user, id, title, aiSummary, userUnderstanding); err != nil {
		return err
	}
	// 事件携带更新后的条目快照（重读一次，保证时间戳/内容为落库真值）
	if item, _, gerr := s.Get(ctx, app, user, id); gerr == nil {
		s.emit(KnowledgeEvent{Kind: EventUpdated, Item: item})
	}
	return nil
}

// SetStatus 归档 / 重新打开。
func (s *Service) SetStatus(ctx context.Context, app, user, id, status string) error {
	if status != storage.KnowledgeStatusActive && status != storage.KnowledgeStatusArchived {
		return fmt.Errorf("%w: invalid status %q", ErrValidation, status)
	}
	if _, _, err := s.Get(ctx, app, user, id); err != nil {
		return err
	}
	if err := s.st.SetKnowledgeStatus(ctx, app, user, id, status); err != nil {
		return err
	}
	if item, _, gerr := s.Get(ctx, app, user, id); gerr == nil {
		s.emit(KnowledgeEvent{Kind: EventUpdated, Item: item})
	}
	return nil
}

// Delete 硬删除（连同全部来源，存储层同事务）；目标不存在为幂等成功（不发事件）。
func (s *Service) Delete(ctx context.Context, app, user, id string) error {
	// 先取快照：删除事件需携带条目内容；不存在时静默幂等成功
	item, _, err := s.Get(ctx, app, user, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if err := s.st.DeleteKnowledge(ctx, app, user, id); err != nil {
		return err
	}
	s.emit(KnowledgeEvent{Kind: EventDeleted, Item: item})
	return nil
}

// dedupKey 幂等锚点：sha256(origin_session_id ‖ origin_event_id)。
// 同一回答事件重复提交必然得到同一键，唯一索引在存储层拦截。
func dedupKey(originSessionID, originEventID string) string {
	sum := sha256.Sum256([]byte(originSessionID + "\x00" + originEventID))
	return hex.EncodeToString(sum[:])
}

func newID() string { return uuid.NewString() }
