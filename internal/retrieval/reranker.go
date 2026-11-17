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
	"sort"
	"sync"
	"time"

	"absorb-agent/internal/log"
	"absorb-agent/internal/store/storage"
	fetch "absorb-agent/tool/fetch_page"
)

// Reranker 精排接缝：对候选文档按与 query 的相关度打分（分数越高越相关）。
// 返回值与 docs 一一对应；失败由调用方决定降级策略（保持原序）。
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]float64, error)
}

// 共享实例（与 SharedEmbedder 同模式）：装配层启动时 Set 一次，检索链路取用；
// 未设置 = 禁用精排（RRF 融合序直接进多样性层）。
var (
	sharedRerankMu   sync.RWMutex
	sharedReranker   Reranker
	sharedRerankTopN = 8
	sharedRerankSetN bool
)

// SetSharedReranker 装配层注入共享精排器；candidates 指定精排候选数（<=0 取默认 8）。
func SetSharedReranker(r Reranker, candidates int) {
	sharedRerankMu.Lock()
	defer sharedRerankMu.Unlock()
	sharedReranker = r
	if candidates > 0 {
		sharedRerankTopN = candidates
		sharedRerankSetN = true
	}
}

// SharedReranker 取共享精排器；未设置返回 nil（检索链路自动跳过精排）。
// SharedRerankTopN 返回精排候选数。
func SharedReranker() Reranker {
	sharedRerankMu.RLock()
	defer sharedRerankMu.RUnlock()
	return sharedReranker
}

func SharedRerankTopN() int {
	sharedRerankMu.RLock()
	defer sharedRerankMu.RUnlock()
	if sharedReranker == nil && !sharedRerankSetN {
		return sharedRerankTopN
	}
	return sharedRerankTopN
}

// LlamaRerankClient /v1/rerank 端点客户端：local sidecar（llama-server）与
// 云 API（硅基流动等，同款 Jina/Cohere 风格请求）共用——差别只在 baseURL、
// model 与鉴权（local apiKey 为空，云传 Bearer Key）。
type LlamaRerankClient struct {
	baseURL string
	model   string
	apiKey  string
	proxyFn func() string // 云模式出网代理（local 直连忽略）
	hc      *http.Client
}

// NewLlamaRerankClient 构建 rerank 客户端。baseURL 形如 http://127.0.0.1:8937/v1
// 或 https://api.siliconflow.cn/v1；apiKey 为空 = 不带鉴权（local）。
func NewLlamaRerankClient(baseURL, model, apiKey string, proxyFn func() string) *LlamaRerankClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = fetch.ProxyResolver(proxyFn)
	return &LlamaRerankClient{
		baseURL: trimSlash(baseURL),
		model:   model,
		apiKey:  apiKey,
		proxyFn: proxyFn,
		hc:      &http.Client{Transport: tr, Timeout: 120 * time.Second}, // CPU 精排一批可达数十秒
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
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
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

// RerankCandidates 检索链路精排步骤：融合候选前 topN 条按相关度降序重排，
// 其余保持融合序缀后。rerank 失败降级融合序（宁可用融合排名也不丢结果），
// 返回 applied=false 供调用方观测（span 属性 / 评测口径）；r == nil 视为
// 未配置精排。检索链路入口统一取 SharedReranker。
func RerankCandidates(ctx context.Context, fused []storage.DocumentChunkHit, query string, r Reranker, topN int) ([]storage.DocumentChunkHit, bool) {
	if r == nil || len(fused) == 0 {
		return fused, false
	}
	if topN <= 0 || topN > len(fused) {
		topN = len(fused)
	}
	docs := make([]string, topN)
	for i := 0; i < topN; i++ {
		docs[i] = fused[i].Content
	}
	scores, err := r.Rerank(ctx, query, docs)
	if err != nil {
		log.Warnf("[Absorb] rerank failed, fallback to fusion order (q=%q): %v", query, err)
		return fused, false
	}
	if len(scores) != topN {
		log.Warnf("[Absorb] rerank score count mismatch (%d != %d), fallback to fusion order (q=%q)",
			len(scores), topN, query)
		return fused, false
	}
	ord := make([]int, topN)
	for i := range ord {
		ord[i] = i
	}
	sort.SliceStable(ord, func(a, b int) bool { return scores[ord[a]] > scores[ord[b]] })
	out := make([]storage.DocumentChunkHit, 0, len(fused))
	for _, id := range ord {
		out = append(out, fused[id])
	}
	return append(out, fused[topN:]...), true
}
