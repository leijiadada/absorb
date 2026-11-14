// Absorb 阅读助手 - 常量配置
// 桌面端问答桥端口由 OS 动态分配（运行时经 SW 发现），此处不再有写死的服务地址。
export const STORAGE_KEY = 'absorb-state';
export const SETTINGS_KEY = 'absorb-settings';
export const DRAFT_KEY = 'absorb-draft';   // 输入框草稿（序列化快照，关面板不丢）
// 文档卡状态表（PDF 解读 PR3）：documentId(=URL) → {state, charCount, ...}。
// 只持久化终态（ready/error）；reading 是内存态（面板关闭 fetch 即中断，恢复无意义）
export const DOCCARDS_KEY = 'absorb-doccards';
// 桌面端下载落地页（未安装引导卡片跳转）：指向 releases 页而非直链安装包，
// 版本不写死在扩展里。GitHub 迁移完成后切换为 https://github.com/vleij/absorb/releases
export const DESKTOP_DOWNLOAD_URL = 'https://gitee.com/leij1/absorb/releases';
