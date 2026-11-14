package knowledge

import (
	"absorb-agent/internal/store/storage"
)

// 知识事件种类（SSE type 字段值，由桥接层透出给前端刷新知识列表）。
const (
	EventCreated = "knowledge.created"
	EventUpdated = "knowledge.updated"
	EventDeleted = "knowledge.deleted"
)

// KnowledgeEvent 知识变更事件：Service 在成功写入后向 onChange 出口发出的极薄通知。
// Item 为变更后的条目快照（deleted 为删除前快照）。
type KnowledgeEvent struct {
	Kind string                  `json:"type"`
	Item *storage.KnowledgeItem `json:"item"`
}

// SetOnChange 注册变更事件出口（可选；nil 关闭）。由宿主在装配时调用一次，
// Service 不知道 SSE 的存在——回调只做进程内投递（如桥接层广播器）。
// 回调在业务请求 goroutine 上同步执行，实现方须自行保证非阻塞。
func (s *Service) SetOnChange(fn func(KnowledgeEvent)) { s.onChange = fn }

// emit 事件出口投递（未注册回调时为空操作）。
func (s *Service) emit(ev KnowledgeEvent) {
	if s.onChange != nil {
		s.onChange(ev)
	}
}
