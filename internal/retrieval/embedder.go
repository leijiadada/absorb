// Package retrieval M3 混合检索：Embedder 接缝（OpenAI 兼容 /v1/embeddings，
// 硅基流动 bge-m3 等）+ RRF 双路融合。与 internal/document（词法索引）解耦，
// 由 search_document 工具编排双路。
package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	fetch "absorb-agent/tool/fetch_page"
)

// 共享实例（与 document.SharedAnalyzer 同模式）：embedding 配置进程级不变
// （重启生效），装配层启动时 Set 一次，检索链路各处直接取用；未设置 = 禁用。
var (
	sharedMu    sync.RWMutex
	sharedEmbed Embedder
)

// SetSharedEmbedder 装配层注入共享实例（云 API 或本地 sidecar 包装），nil = 禁用。
func SetSharedEmbedder(e Embedder) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	sharedEmbed = e
}

// SharedEmbedder 取共享实例；未设置返回 nil（检索链路自动纯词法）。
func SharedEmbedder() Embedder {
	sharedMu.RLock()
	defer sharedMu.RUnlock()
	return sharedEmbed
}

// Embedder 语义向量化接缝：索引期批量、查询期单条。实现须可并发安全。
type Embedder interface {
	Model() string
	// EmbedDocuments 批量向量化（索引期）。返回顺序与输入一致。
	EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQuery 查询向量化。部分模型对查询/文档用不同前缀，由实现内部处理。
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// OpenAICompatEmbedder OpenAI 兼容 /v1/embeddings 客户端（硅基流动等）。
// 复用升级链路的代理解析（ProxyResolver），保证全应用出网代理一致。
type OpenAICompatEmbedder struct {
	baseURL  string
	apiKey   string
	model    string
	batch    int
	maxChars int // 单条输入字符上限（0=不截断）
	hc       *http.Client
}

// NewOpenAICompatEmbedder 构建客户端。proxyFn 复用全局代理解析（可 nil=直连）。
func NewOpenAICompatEmbedder(baseURL, apiKey, model string, batchSize int, proxyFn func() string) *OpenAICompatEmbedder {
	if baseURL == "" {
		baseURL = "https://api.siliconflow.cn/v1"
	}
	if model == "" {
		model = "BAAI/bge-m3"
	}
	if batchSize <= 0 {
		batchSize = 32
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = fetch.ProxyResolver(proxyFn)
	return &OpenAICompatEmbedder{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		batch:   batchSize,
		hc:      &http.Client{Transport: tr, Timeout: 60 * time.Second},
	}
}

func (e *OpenAICompatEmbedder) Model() string { return e.model }

// WithMaxChars 单条输入字符级截断上限（链式）。bge-v1.5 系训练上下文 512 token
// （中文 ≈1 token/字），超长部分本就无效；llama-server 对超 n_ctx 输入直接拒绝
// 而非截断，local 模式由客户端截断兜底（云 API 有 truncate:"right"，双保险无害）。
func (e *OpenAICompatEmbedder) WithMaxChars(n int) *OpenAICompatEmbedder {
	e.maxChars = n
	return e
}

// EmbedDocuments 分批调用 /v1/embeddings（input 数组）。批间失败快速上抛——
// 调用方（BuildIndex）对失败整体降级词法，不做批级重试（限流场景由下次 prepare 重试）。
func (e *OpenAICompatEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += e.batch {
		end := start + e.batch
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.call(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (e *OpenAICompatEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.call(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	return vecs[0], nil
}

// call 单批请求。注意：bge-*-v1.5 large 系对短查询建议加指令前缀
// （"为这个句子生成表示以用于检索相关文章："），默认的 bge-m3 无此要求——
// 若后续把默认模型换成 v1.5 系，EmbedQuery 需补前缀。
func (e *OpenAICompatEmbedder) call(ctx context.Context, texts []string) ([][]float32, error) {
	// 字符级截断（WithMaxChars）：bge-v1.5 系 512 token 训练上下文的客户端兜底
	if e.maxChars > 0 {
		trimmed := make([]string, len(texts))
		for i, t := range texts {
			if r := []rune(t); len(r) > e.maxChars {
				t = string(r[:e.maxChars])
			}
			trimmed[i] = t
		}
		texts = trimmed
	}
	payload, err := json.Marshal(map[string]any{
		"model":           e.model,
		"input":           texts,
		"encoding_format": "float",
		"truncate":        "right", // 超模型上下文时右侧截断（防御：bge-m3 上限 8192 token，正常 chunk 远低于此）
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding api %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}
	var parsed struct {
		Data []struct {
			Index int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("embedding response missing index %d", i)
		}
	}
	return out, nil
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
