package kind

// DocType 标识提问来源的材料形态，供 prompt 侧来源标注与工具引导分叉。
// 空串 "" 表示未判定（普通网页划词/提问，未走文档通道），按网页口径渲染。
type DocType string

const (
	DocTypeWeb DocType = "web" // 网页形态：普通页面正文（经 prepare 提取的 HTML 文档亦归此）
	DocTypePDF DocType = "pdf" // PDF 文档：application/pdf 或 .pdf 来源经提取的正文
)

// Valid 报告 d 是否为已知材料形态（空串代表未判定，也视为合法）。
func (d DocType) Valid() bool {
	switch d {
	case "", DocTypeWeb, DocTypePDF:
		return true
	default:
		return false
	}
}
