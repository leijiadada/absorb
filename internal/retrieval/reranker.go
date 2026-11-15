// Reranker 精排接缝（M3）：对召回候选按「查询-文档相关性」重排。
// 与 Embedder 同模式：接口在检索链路消费，实现走 llama-server /v1/rerank
// （--reranking 模式，bge-reranker-v2-m3 等 sequence-classification GGUF）。
package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Reranker 精排接缝：对候选文档按与 query 的相关度打分（分数越高越相关）。
// 返回值与 docs 一一对应；失败由调用方决定降级策略（保持原序）。
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]float64, error)
}

// LlamaRerankClient llama-server /v1/rerank 端点客户端（local sidecar）。
type LlamaRerankClient struct {
	baseURL string
	model   string
	hc      *http.Client
}

// NewLlamaRerankClient 构建 rerank 客户端。baseURL 形如 http://127.0.0.1:8937/v1。
func NewLlamaRerankClient(baseURL, model string) *LlamaRerankClient {
	return &LlamaRerankClient{
		baseURL: trimSlash(baseURL),
		model:   model,
		hc:      &http.Client{Timeout: 120 * time.Second}, // CPU 精排一批可达数十秒
	}
}

// Rerank 调 /v1/rerank（Jina/Cohere 风格）：按输入顺序返回每条文档的相关分。
func (c *LlamaRerankClient) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(map[string]any{
		"model":     c.model,
		"query":     query,
		"documents": docs,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/rerank", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank api %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}
	var parsed struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	// 响应按分数降序，还原回输入顺序；未返回的位置视为最低分
	out := make([]float64, len(docs))
	for i := range out {
		out[i] = -1e9
	}
	for _, r := range parsed.Results {
		if r.Index >= 0 && r.Index < len(out) {
			out[r.Index] = r.RelevanceScore
		}
	}
	return out, nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
