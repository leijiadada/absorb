package profile

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	// 纯 Go SQLite 驱动（驱动名经 internal/store 注册，测试直连时需自带）
	_ "modernc.org/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	memorystore "trpc.group/trpc-go/trpc-agent-go/memory/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/model"

	"absorb-agent/internal/conf"
)

// TestProfileWatermarkPersistence 水位文件读写回环。
func TestProfileWatermarkPersistence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "profile.state.json")
	if st := loadProfileState(path); st.LastCount != 0 {
		t.Fatalf("缺省水位应为 0, got %d", st.LastCount)
	}
	if err := saveProfileState(path, profileState{LastCount: 42, UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("saveProfileState: %v", err)
	}
	if st := loadProfileState(path); st.LastCount != 42 {
		t.Fatalf("回读 LastCount=%d; want 42", st.LastCount)
	}
}

// TestProfileShouldDistill 蒸馏水位判定：首蒸有记忆即触发；已有画像须达水位。
func TestProfileShouldDistill(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		count, lastCount int
		hasProfile       bool
		minNew           int
		want             bool
	}{
		{"无记忆不蒸", 0, 0, false, 5, false},
		{"首蒸有记忆即蒸", 3, 0, false, 5, true},
		{"未达水位不蒸", 7, 5, true, 5, false},
		{"达水位即蒸", 10, 5, true, 5, true},
		{"记忆回退不蒸", 4, 10, true, 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldDistillProfile(c.count, c.lastCount, c.hasProfile, c.minNew); got != c.want {
				t.Errorf("shouldDistillProfile(%d,%d,%v,%d) = %v, want %v",
					c.count, c.lastCount, c.hasProfile, c.minNew, got, c.want)
			}
		})
	}
}

// TestProfileAtomicWrite 原子写：首次创建与覆盖更新均落盘正确。
func TestProfileAtomicWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "profile.md")
	if err := writeProfileAtomic(path, "第一版画像"); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if err := writeProfileAtomic(path, "第二版画像"); err != nil {
		t.Fatalf("覆盖写入: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "第二版画像" {
		t.Fatalf("content=%q err=%v; want 第二版画像", string(b), err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("临时文件应已清理: %v", err)
	}
}

// TestProfileOutputClamp 超长蒸馏输出收敛到段落边界；未超限原样返回。
func TestProfileOutputClamp(t *testing.T) {
	t.Parallel()
	if got := clampProfileOutput("短画像"); got != "短画像" {
		t.Fatalf("未超限应原样返回, got %q", got)
	}
	// 1000 行 × 4 runes = 4000 runes + 无换行尾部 200 runes，确保超过 maxProfileOutputRunes(3500)
	long := strings.Repeat("第一段\n", 1000) + strings.Repeat("尾巴", 100)
	got := clampProfileOutput(long)
	if n := len([]rune(got)); n > maxProfileOutputRunes {
		t.Fatalf("截断后仍超限: %d", n)
	}
	if strings.HasSuffix(got, "尾") || strings.Contains(got, "尾巴尾巴尾巴尾") {
		t.Fatalf("应在段落边界截断, got tail %q", got[max(0, len(got)-30):])
	}
}

// stubDistillModel 最小蒸馏模型桩：单帧非流式应答，记录调用次数（跳过路径断言用）。
type stubDistillModel struct {
	mu       sync.Mutex
	calls    int
	response string
}

func (m *stubDistillModel) Info() model.Info { return model.Info{Name: "stub"} }

func (m *stubDistillModel) GenerateContent(_ context.Context, _ *model.Request) (<-chan *model.Response, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	ch := make(chan *model.Response, 1)
	ch <- &model.Response{
		Done: true,
		Choices: []model.Choice{{
			Message: model.Message{Role: model.RoleAssistant, Content: m.response},
		}},
	}
	close(ch)
	return ch, nil
}

// newDistillMemoryService 真实记忆库（sqlite + t.TempDir 隔离，无需 build tag）。
func newDistillMemoryService(t *testing.T) memory.Service {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "memory.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
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

// TestProfileDistillWithStubModel 蒸馏主路径（stub 模型，非真实 LLM）：
// 首蒸落盘与水位推进 → 无新增跳过且不再调模型 → 未达水位跳过 →
// 达水位再蒸且超长输出截断到段落边界。
func TestProfileDistillWithStubModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	uk := memory.UserKey{AppName: "absorb-reader", UserID: "distill-stub-user"}
	svc := newDistillMemoryService(t)
	pc := conf.ProfileConfig{FilePath: filepath.Join(t.TempDir(), "profile.md"), MinNewEntries: 5}
	m := &stubDistillModel{response: "用户是后端工程师，偏好简短回答"}
	for _, fact := range []string{"用户是后端工程师", "用户偏好简短回答"} {
		if err := svc.AddMemory(ctx, uk, fact, nil); err != nil {
			t.Fatalf("AddMemory: %v", err)
		}
	}

	// 首蒸：无画像 + count>0 → 调模型并落盘，水位推进到真实总数
	if err := distillProfile(ctx, svc, uk, pc, m); err != nil {
		t.Fatalf("首蒸: %v", err)
	}
	b, err := os.ReadFile(pc.FilePath)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		t.Fatalf("画像文件未生成或为空: err=%v content=%q", err, b)
	}
	if st := loadProfileState(profileStatePath(pc.FilePath)); st.LastCount != 2 {
		t.Fatalf("水位应为截断前总数 2, got %d", st.LastCount)
	}

	// 无新增：跳过且不再调模型
	if err := distillProfile(ctx, svc, uk, pc, m); err != nil {
		t.Fatalf("无新增应静默跳过: %v", err)
	}
	if m.calls != 1 {
		t.Fatalf("无新增不应再次调用模型, calls=%d", m.calls)
	}

	// 新增 1 条未达水位：跳过
	if err := svc.AddMemory(ctx, uk, "用户新增的独立事实", nil); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	if err := distillProfile(ctx, svc, uk, pc, m); err != nil {
		t.Fatalf("未达水位应静默跳过: %v", err)
	}
	if m.calls != 1 {
		t.Fatalf("未达水位不应调用模型, calls=%d", m.calls)
	}

	// 再新增 5 条达水位：再蒸，超长输出被截断到段落边界
	m.mu.Lock()
	m.response = strings.Repeat("第一段\n", 250) // 1000 runes > 800
	m.mu.Unlock()
	for i := 0; i < 5; i++ {
		if err := svc.AddMemory(ctx, uk, "用户学习轨迹条目"+string(rune('A'+i)), nil); err != nil {
			t.Fatalf("AddMemory: %v", err)
		}
	}
	if err := distillProfile(ctx, svc, uk, pc, m); err != nil {
		t.Fatalf("达水位再蒸: %v", err)
	}
	content, err := os.ReadFile(pc.FilePath)
	if err != nil {
		t.Fatalf("读取画像: %v", err)
	}
	if n := len([]rune(string(content))); n > maxProfileOutputRunes {
		t.Fatalf("超长输出未截断: %d runes", n)
	}
	if !strings.HasSuffix(string(content), "第一段") {
		t.Fatalf("应截断到段落边界(完整行), tail=%q", content[max(0, len(content)-30):])
	}
}
