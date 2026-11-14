package kind

// ScopeGeneral 无页面轮次的通用 scope 标识：chat 层在无划词、无页面兜底、历史亦
// 无可继承 URL 时（如浏览器新标签页提问），把本轮事件统一打在此 scope。
// 不留空标（FilterKey==""）——空标事件会被摘要增量计入每一个文章 scope 并随任意
// scope 的 cutoff 被裁，通用内容因此以压缩形态混入所有文章的摘要卡；跨文章可见性
// 由工作集 L1 承接（general 轮对文章请求 Filter=false，L1 保留注入原文折叠）。
// 另消除一处隐式碰撞：SDK 的 SummaryFilterKeyAllContents==""，空标轮次摘要与级联
// 全会话摘要同键会互相覆写。
//
// 定名约束（与 store.ScopeID 哈希键、SDK 前缀语义的跨层契约）：不含 "/"、含非
// 十六进制字符（'n'/'r'/'l'），故不是任何 16 位哈希键的前缀或本身，与
// event.Filter / SummaryFilterKeyMatchesPrefix 的前缀匹配无碰撞。
const ScopeGeneral = "general"

// ScopeGeneralDesc ScopeGeneral 在 scope_urls 映射里的人类可读占位（日志/调试）。
const ScopeGeneralDesc = "（通用对话·无页面）"
