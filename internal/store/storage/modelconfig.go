package storage

import "time"

// ModelConfig model_configs 表行。Key 明文存储（与原 config.yaml 一致），
// 回传前端前须在绑定层脱敏。Temperature/MaxTokens 为 nil 表示跟随模型侧默认。
// Thinking 三态：0=跟随模型默认（请求不下发参数） 1=开启 2=关闭。
type ModelConfig struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`    // bigmodel/deepseek/kimi/.../custom
	DisplayName  string    `json:"displayName"` // 展示名称，空=显示 ModelID
	ModelID      string    `json:"modelId"`     // 真实模型 ID，如 glm-4.6
	APIFormat    string    `json:"apiFormat"`   // V1 固定 openai_chat，V2 原生协议预留
	BaseURL      string    `json:"baseUrl"`
	FullURL      bool      `json:"fullUrl"` // 1=BaseURL 即完整端点，不再自动补 /chat/completions
	APIKey       string    `json:"apiKey"`
	Temperature  *float64  `json:"temperature"`
	MaxTokens    *int      `json:"maxTokens"`
	Thinking     int       `json:"thinking"`
	ToolRounds   *int      `json:"toolRounds"`   // 工具调用轮数上限，nil=框架默认；暂为元数据（MaxToolIterations 为 agent 构造期参数）
	ImageSupport bool      `json:"imageSupport"` // 支持图片输入，元数据（多模态预留）
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}
