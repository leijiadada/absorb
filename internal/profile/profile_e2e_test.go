//go:build realmodel

package profile

// 真实 GLM 蒸馏端到端探针：写入原子记忆 → distillProfile → 画像文件生成、二次蒸馏跳过。
//
// 双重门控（真实 API 会消耗额度）：需 build tag `realmodel` 且环境变量 ABSORB_REAL_MODEL 非空。
// 运行：ABSORB_REAL_MODEL=1 go test -tags=realmodel ./internal/profile/ -run TestProfileDistillWithRealLLM -v
//
// 断言为契约级：只验证「画像文件生成且非空」「二次蒸馏静默跳过」，
// 不校验 LLM 的具体措辞——输出不可控，断言文案会造成 flaky。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/memory"
	memorystore "trpc.group/trpc-go/trpc-agent-go/memory/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"

	// 纯 Go SQLite 驱动（驱动名经 internal/store 注册，测试直连时需自带）
	_ "modernc.org/sqlite"

	"absorb-agent/internal/conf"
)

// newE2EModel 按生产同款参数构建 GLM 模型（与问答模型配置一致）。
func newE2EModel(cfg conf.Config) model.Model {
	return openai.New(cfg.Model.Name,
		openai.WithVariant(openai.Variant(cfg.Model.Variant)),
		openai.WithBaseURL(cfg.Model.BaseURL),
		openai.WithAPIKey(cfg.Model.APIKey),
		openai.WithTextOnlyMessageContent(true),
	)
}

// newE2EMemoryService 打开独立记忆库（蒸馏用例只需 AddMemory/ReadMemories 基础能力）。
func newE2EMemoryService(t *testing.T, dbPath string) memory.Service {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开记忆库: %v", err)
	}
	svc, err := memorystore.NewService(db)
	if err != nil {
		t.Fatalf("初始化记忆服务: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// TestProfileDistillWithRealLLM 真实 GLM 蒸馏：写入原子记忆 → distillProfile →
// 画像文件生成，二次蒸馏应因无新增而静默跳过。
func TestProfileDistillWithRealLLM(t *testing.T) {
	if os.Getenv("ABSORB_REAL_MODEL") == "" {
		t.Skip("set ABSORB_REAL_MODEL=1 to run real LLM distillation probe")
	}
	cfg := conf.LoadConfig("../../config/config.yaml")
	tmp := t.TempDir()
	cfg.Memory.DBPath = filepath.Join(tmp, "memory.db")
	cfg.Memory.Profile.FilePath = filepath.Join(tmp, "profile.md")

	m := newE2EModel(cfg)
	svc := newE2EMemoryService(t, cfg.Memory.DBPath)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	uk := memory.UserKey{AppName: "absorb-reader", UserID: "profile-e2e-user"}
	for _, fact := range []string{
		"用户是后端工程师，主要使用 Go 语言",
		"用户偏好简短回答，先结论后例子",
		"用户正在学习 Go 并发编程",
	} {
		if err := svc.AddMemory(ctx, uk, fact, nil); err != nil {
			t.Fatalf("AddMemory: %v", err)
		}
	}

	pc := cfg.Memory.Profile
	if err := distillProfile(ctx, svc, uk, pc, m); err != nil {
		t.Fatalf("distillProfile: %v", err)
	}
	b, err := os.ReadFile(pc.FilePath)
	if err != nil {
		t.Fatalf("画像文件未生成: %v", err)
	}
	profile := string(b)
	t.Logf("蒸馏画像:\n%s", profile)
	// 契约级断言：只要求蒸馏产出非空画像，不锁定 LLM 的具体措辞。
	if strings.TrimSpace(profile) == "" {
		t.Fatalf("画像为空，蒸馏未产出内容")
	}

	// 水位推进：再次蒸馏应因无新增而跳过（不报错）
	if err := distillProfile(ctx, svc, uk, pc, m); err != nil {
		t.Fatalf("二次蒸馏应静默跳过: %v", err)
	}
}
