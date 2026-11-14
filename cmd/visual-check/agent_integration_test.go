//go:build integration

// visual-explanation 链路端到端回归（真实 LLM）。
// 跑法：ABSORB_E2E=1 go test -tags=integration ./cmd/visual-check/ -v
// 未设置 ABSORB_E2E 时自动 skip（会产生真实 API 调用费用，必须显式开启）。
// 本轮回归串行执行：真实模型有速率限制，并发只会引入 provider 报错与超时噪声。
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// caseTimeout 单用例超时：真实模型偶发卡住时快速失败，而不是拖到 go test 默认超时。
const caseTimeout = 180 * time.Second

// configPath 集成测试视角的配置路径：go test 工作目录为包目录，需上溯到仓库根；
// 可用 ABSORB_CONFIG 覆盖。
func configPath() string {
	if p := os.Getenv("ABSORB_CONFIG"); p != "" {
		return p
	}
	return filepath.Join("..", "..", "config", "config.yaml")
}

func TestRegressionCases(t *testing.T) {
	if os.Getenv("ABSORB_E2E") == "" {
		t.Skip("设置 ABSORB_E2E=1 才运行真实 LLM 端到端回归（会产生 API 调用费用）")
	}

	absCfg, err := filepath.Abs(configPath())
	if err != nil {
		t.Fatalf("解析配置路径: %v", err)
	}
	if _, err := os.Stat(absCfg); err != nil {
		t.Skipf("未找到 %s，跳过集成回归（可用 ABSORB_CONFIG 指定路径）", absCfg)
	}
	// 配置中的相对路径以仓库根为基准；测试工作目录是包目录，必须先切回仓库根。
	if _, err := chdirRepoRoot(absCfg); err != nil {
		t.Fatalf("切换到仓库根失败: %v", err)
	}

	for _, tt := range regressionCases() {
		t.Run(tt.id, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), caseTimeout)
			defer cancel()

			env, err := newCheckEnv(absCfg, false)
			if err != nil {
				t.Fatal(err)
			}
			defer env.closer()

			answer, err := env.ask(ctx, tt.question, "cli-"+tt.id)
			if err != nil {
				t.Fatalf("调用失败: %v", err)
			}
			if reason := providerErrorReason(answer); reason != "" {
				t.Fatalf("上游未返回有效回答（%s）: %q", reason, answer)
			}

			// 回答全文落盘为测试产物，失败时人工排查原始输出
			out := filepath.Join(t.ArtifactDir(), tt.id+".md")
			if err := os.WriteFile(out, []byte(answer), 0o644); err != nil {
				t.Fatalf("写入产物: %v", err)
			}
			t.Logf("回答 %d 字，落盘 %s", len(answer), out)

			ok, asserts := tt.evaluate(answer)
			if !ok {
				// 失败时固化完整回答：ArtifactDir 随测试进程清理（实测两次被清），
				// 固定路径落盘供排查；片段随失败信息就地输出。
				dump := filepath.Join(os.TempDir(), "absorb-visual-fail-"+tt.id+".md")
				_ = os.WriteFile(dump, []byte(answer), 0o644)
				t.Logf("完整回答已固化: %s", dump)
				excerpt := answer
				if len(excerpt) > 500 {
					excerpt = excerpt[:500] + "…"
				}
				t.Errorf("[%s] 断言失败: %v\n回答开头: %s", tt.id, asserts, excerpt)
			}
		})
	}
}

// providerErrorReason 识别"上游报错被当成回答"的响应：这类内容长度极短、
// 断言只会报 visual 缺失，必须在断言前单独判定，避免误导排查方向。
func providerErrorReason(answer string) string {
	if strings.TrimSpace(answer) == "" {
		return "空回答"
	}
	for _, marker := range []string{
		"An error occurred during execution",
		"Please contact the service provider",
	} {
		if strings.Contains(answer, marker) {
			return "provider 错误"
		}
	}
	return ""
}
