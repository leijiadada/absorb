package chat

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // 记忆库驱动（internal/store 已注册同名驱动，此处显式声明依赖）
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/memory/extractor"
	memorystore "trpc.group/trpc-go/trpc-agent-go/memory/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"

	"absorb-agent/internal/conf"
)

// MemoryGate 记忆功能运行时总开关：记忆服务、画像注入、画像蒸馏三方共用同一实例。
// 设置页保存时翻转此开关即热切换（无需重启）：结构资源（DB 连接、worker）常驻，
// 仅行为短路。新增"开关类"配置照此模式预留门控点即可。
type MemoryGate struct {
	on atomic.Bool
}

func NewMemoryGate(initial bool) *MemoryGate {
	g := &MemoryGate{}
	g.on.Store(initial)
	return g
}

func (g *MemoryGate) Enabled() bool { return g.on.Load() }

// Set 翻转开关（保存落盘成功后由设置层调用）。
func (g *MemoryGate) Set(on bool) { g.on.Store(on) }

// gatedMemoryService 记忆服务门控包装：关闭态短路读写与提取，开启态行为与底层服务完全一致。
// 内嵌 memory.Service 接口透传未覆写方法（Tools/Close 等）。
type gatedMemoryService struct {
	memory.Service
	gate *MemoryGate
}

func (s *gatedMemoryService) ReadMemories(ctx context.Context, userKey memory.UserKey, limit int) ([]*memory.Entry, error) {
	if !s.gate.Enabled() {
		return nil, nil
	}
	return s.Service.ReadMemories(ctx, userKey, limit)
}

func (s *gatedMemoryService) SearchMemories(ctx context.Context, userKey memory.UserKey, query string, opts ...memory.SearchOption) ([]*memory.Entry, error) {
	if !s.gate.Enabled() {
		return nil, nil
	}
	return s.Service.SearchMemories(ctx, userKey, query, opts...)
}

func (s *gatedMemoryService) AddMemory(ctx context.Context, userKey memory.UserKey, mem string, topics []string, opts ...memory.AddOption) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.AddMemory(ctx, userKey, mem, topics, opts...)
}

func (s *gatedMemoryService) UpdateMemory(ctx context.Context, memoryKey memory.Key, mem string, topics []string, opts ...memory.UpdateOption) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.UpdateMemory(ctx, memoryKey, mem, topics, opts...)
}

func (s *gatedMemoryService) DeleteMemory(ctx context.Context, memoryKey memory.Key) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.DeleteMemory(ctx, memoryKey)
}

func (s *gatedMemoryService) ClearMemories(ctx context.Context, userKey memory.UserKey) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.ClearMemories(ctx, userKey)
}

func (s *gatedMemoryService) EnqueueAutoMemoryJob(ctx context.Context, sess *session.Session) error {
	if !s.gate.Enabled() {
		return nil
	}
	return s.Service.EnqueueAutoMemoryJob(ctx, sess)
}

// newMemoryService 构建长期记忆服务（常驻）：SQLite 后端（独立子模块，replace 引入）+ LLM 提取器（auto 模式），
// 外层包门控——enabled 是运行时开关（热切换），不再决定服务是否装配。
// 返回 (nil, warn) 表示初始化失败，调用方降级为无记忆运行。
func newMemoryService(cfg conf.Config, m model.Model, gate *MemoryGate) (memory.Service, string) {
	db, err := sql.Open("sqlite",
		cfg.Memory.DBPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, "打开记忆库失败，记忆功能停用: " + err.Error()
	}
	svcOpts := []memorystore.ServiceOpt{
		// auto 模式：runner 响应结束后由框架异步提取事实写入记忆库
		memorystore.WithExtractor(extractor.NewExtractor(m)),
		// 记忆提取含 LLM 调用，耗时高于默认 30s
		memorystore.WithMemoryJobTimeout(2 * time.Minute),
		memorystore.WithAsyncMemoryNum(cfg.Memory.AsyncNum),
		memorystore.WithMemoryQueueSize(cfg.Memory.QueueSize),
		memorystore.WithSoftDelete(true), // 删除走墓碑
	}
	svc, err := memorystore.NewService(db, svcOpts...)
	if err != nil {
		_ = db.Close()
		return nil, "初始化记忆服务失败，记忆功能停用: " + err.Error()
	}
	return &gatedMemoryService{Service: svc, gate: gate}, ""
}
