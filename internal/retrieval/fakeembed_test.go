package retrieval

// 假 embedding 端点：按输入文本确定性生成向量（首字符码点决定方向），
// 供 httptest 服务与单测共用。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeVectorFor 确定性向量：同一文本恒定，不同首字符方向不同（余弦可分辨）。
func fakeVectorFor(text string) []float32 {
	v := make([]float32, 4)
	if text == "" {
		return v
	}
	v[int(text[0])%4] = 1
	if len(text) > 1 {
		v[int(text[1])%4] = 0.5
	}
	return v
}

// newEmbedTestServer 起 OpenAI 兼容 /embeddings 假端点，记录收到的批。
func newEmbedTestServer(t *testing.T, batches *[][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*batches = append(*batches, req.Input)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, len(req.Input))
		for i, in := range req.Input {
			data[i] = item{Index: i, Embedding: fakeVectorFor(in)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}
