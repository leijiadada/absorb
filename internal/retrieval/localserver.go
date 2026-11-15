// 本地 embedding sidecar（M3，provider=local）：管理 llama-server 子进程
// （--embeddings 模式，OpenAI 兼容 /v1/embeddings），供共享 Embedder 复用
// 同一 OpenAI 兼容客户端——local 与云 API 的差别只在 base_url 与进程生命周期。
package retrieval

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"absorb-agent/internal/log"
)

// LocalEmbeddingServer llama-server 子进程管理器。Start 幂等（已就绪直接返回）；
// Stop 终止进程。进程崩溃由下次 Start 重启（不做常驻看护——embedding 失败
// 自动降级词法，重试入口是 prepare/下一轮提问）。同一实现按 mode 兼职管理
// reranker 子进程（--reranking 模式，/v1/rerank 端点）。
type LocalEmbeddingServer struct {
	exe       string
	modelPath string
	port      int
	mode      serverMode
	cmd       *exec.Cmd
}

type serverMode int

const (
	modeEmbedding serverMode = iota
	modeRerank
)

// NewLocalEmbeddingServer 构建子进程管理器（embedding 模式）。
func NewLocalEmbeddingServer(exe, modelPath string, port int) *LocalEmbeddingServer {
	if port <= 0 {
		port = 8935
	}
	return &LocalEmbeddingServer{exe: exe, modelPath: modelPath, port: port, mode: modeEmbedding}
}

// NewRerankServer 构建 reranker 子进程管理器（--reranking 模式，/v1/rerank）。
func NewRerankServer(exe, modelPath string, port int) *LocalEmbeddingServer {
	if port <= 0 {
		port = 8937
	}
	return &LocalEmbeddingServer{exe: exe, modelPath: modelPath, port: port, mode: modeRerank}
}

// BaseURL 子进程的 OpenAI 兼容 base 地址。
func (s *LocalEmbeddingServer) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", s.port)
}

// Start 启动子进程并等待 /health 就绪（llama-server 就绪前请求会 503）。
// 已有存活进程（端口可访问）时跳过启动——支持桌面/CLI 双实例共存于同一模型。
func (s *LocalEmbeddingServer) Start(ctx context.Context) error {
	if s.healthy() {
		return nil
	}
	for _, p := range []string{s.exe, s.modelPath} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("local embedding: %s 不存在", p)
		}
	}
	args := []string{"-m", s.modelPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprint(s.port),
		"--ctx-size", "8192", // 单条 chunk 输入上限（bge-small 上下文 512 token，llama.cpp 按 GGUF 元数据裁剪）
		// physical batch 须 ≥ 单次请求全部 input 的 token 总和（llama-server
		// embeddings 模式不做跨请求切片）：8 条/批 × ~700 token/条 ≈ 5600。
		"--batch-size", "8192",
		"-ngl", "0", // 纯 CPU
	}
	if s.mode == modeRerank {
		// reranker：query-doc pair 打分，端点 /v1/rerank（16 候选 × ~450 token < 8192）
		args = append([]string{"--reranking", "--pooling", "rank"}, args...)
	} else {
		args = append([]string{"--embeddings"}, args...)
	}
	s.cmd = exec.Command(s.exe, args...)
	s.cmd.Stdout = nil
	s.cmd.Stderr = nil
	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("local embedding: 启动 llama-server 失败: %w", err)
	}
	go func() { _ = s.cmd.Wait() }() // 回收进程句柄，避免僵尸

	// 就绪轮询：模型加载（首次 mmap 45MB GGUF）通常 <2s，上限 30s
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s.healthy() {
			log.Infof("[Absorb] local embedding server ready | port=%d model=%s",
				s.port, filepath.Base(s.modelPath))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("local embedding: 服务 30s 未就绪（port=%d）", s.port)
}

// Stop 终止子进程（应用退出时调用；幂等）。
func (s *LocalEmbeddingServer) Stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		s.cmd = nil
	}
}

func (s *LocalEmbeddingServer) healthy() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", s.port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
