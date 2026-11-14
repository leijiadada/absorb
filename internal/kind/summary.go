package kind

// SummaryRecallHint 摘要尾部回溯提示：告知模型"细节已折叠，可经 recall_history
// 工具回溯（先 action=index 查看目录）"。
//
// 跨包契约（chat ↔ store 共用同一字面量，单一事实源在此）：
//   - chat（消费侧）：经 llmagent.WithSummaryFormatter 拼到摘要卡尾部，仅注入
//     模型请求，不持久化；
//   - store（持久化侧）：createSummary 出口剥离模型输出中复制的本提示（防累积），
//     GetSessionSummaryText 拼接后返回。
const SummaryRecallHint = "\n\n（以上为压缩摘要；如需已折叠的原文细节，可调用 recall_history 工具，先 action=index 查看目录）"
