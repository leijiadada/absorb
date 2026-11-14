package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"absorb-agent/internal/store/storage"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

// 文档聚合与阅读摘要（M2.5，总结/吸收分离）：
//   - Document 按 URL 锚定原始资料，knowledge_sources.source_document_id 回链；
//   - 阅读摘要（层次A）是 AI 对文章的概括，存 documents.summary_* 可选字段，
//     与个人知识（层次B）是不同对象——保存摘要永不产生 Knowledge Item。
//
// 来源会话校验（L1）：保存前验证 sessionId 真实存在（storage.GetSession），
// 满足"不创建缺少依据的记录"；事件级校验（L2）依赖 requestID 落库，记技术债。

// ErrOriginSession 来源会话不存在或不可访问（create / saveSummary 共用校验）。
// HTTP 层映射 400 并给固定中文提示。
var ErrOriginSession = fmt.Errorf("%w: origin session unavailable", ErrValidation)

// SaveSummaryInput 保存阅读摘要入参。
type SaveSummaryInput struct {
	SessionID   string // 必填：产生摘要的会话（L1 校验对象）
	RequestID   string // 必填：产生摘要的回答事件（幂等锚点之一）
	URL         string // 必填：文章地址（文档聚合键）
	Title       string // 选填：页面标题（变化时刷新锚点）
	SummaryText string // 必填：AI 总结全文（回答 markdown 原文）
}

// SaveReadingSummary 保存阅读摘要到 URL 对应的文档锚点。
// 幂等：同文档且 summary_event_id 相同（同一回答重复提交）返回 created=false。
// 再次总结后重新保存属合法覆盖（新的 RequestID），非重复。
func (s *Service) SaveReadingSummary(ctx context.Context, app, user string, in SaveSummaryInput) (*storage.Document, bool, error) {
	in.URL = strings.TrimSpace(in.URL)
	in.SessionID = strings.TrimSpace(in.SessionID)
	in.RequestID = strings.TrimSpace(in.RequestID)
	in.SummaryText = strings.TrimSpace(in.SummaryText)
	if in.URL == "" || in.SessionID == "" || in.RequestID == "" || in.SummaryText == "" {
		return nil, false, fmt.Errorf("%w: url/sessionId/requestId/summaryText are required", ErrValidation)
	}
	if err := s.checkOriginSession(ctx, app, user, in.SessionID); err != nil {
		return nil, false, err
	}
	doc, err := s.st.UpsertDocument(ctx, app, user, in.URL, strings.TrimSpace(in.Title))
	if err != nil {
		return nil, false, err
	}
	if doc.SummaryEventID == in.RequestID {
		return doc, false, nil // 幂等命中：同一回答的摘要已保存过
	}
	if err := s.st.UpdateDocumentSummary(ctx, app, user, doc.ID, in.SummaryText, in.SessionID, in.RequestID); err != nil {
		return nil, false, err
	}
	doc.SummaryText = in.SummaryText
	doc.SummarySessionID = in.SessionID
	doc.SummaryEventID = in.RequestID
	return doc, true, nil
}

// checkOriginSession L1 来源校验：origin sessionId 必须在存储中真实存在。
// 空会话 ID 视为缺依据（历史快照可能缺失），同样拒绝；查询性"不存在"
// （sql.ErrNoRows / 空 key）映射 ErrOriginSession，其余错误原样上抛（500），
// 不把存储故障伪装成用户输入问题。
func (s *Service) checkOriginSession(ctx context.Context, app, user, sessionID string) error {
	if sessionID == "" {
		return ErrOriginSession
	}
	sess, err := s.st.GetSession(ctx, session.Key{AppName: app, UserID: user, SessionID: sessionID})
	if err == nil && sess != nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOriginSession
	}
	if err != nil {
		return err
	}
	return ErrOriginSession
}
