package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewModelID 生成模型配置 ID（16 字节随机 hex）。
func NewModelID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("mc-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// triFromBool 旧两态思考开关 → 三态（true=1 开、false=2 关；三态的 0=跟随默认仅新表单产生）。
func triFromBool(b bool) int {
	if b {
		return 1
	}
	return 2
}
