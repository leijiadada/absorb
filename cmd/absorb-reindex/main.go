// absorb-reindex 存量文档全量重建索引 CLI。
//
// chunker 分块规则升级（如 v3 断点优先级细化）后，存量文档的索引仍是旧版本
// 产物——BuildIndex 幂等判断（index_state.ChunkerVersion == 当前值）会跳过
// 它们，只有下次 prepare 才被动重建。本脚本对库内全部 documents 逐个调用
// 生产同款 BuildIndex，立即完成全量重建：
//
//   - 词法域：分块 → 分词 → 事务内整体替换 chunks/FTS（旧向量同事务作废）；
//   - 向量域：-config 提供且配置了 embedding（local sidecar 或云 API）时同步
//     重嵌入；未提供时只建词法，向量域标记为待补建，下次 prepare 自动补。
//
// 幂等可重复运行：已达当前 ChunkerVersion 且 content_hash 未变的文档由
// BuildIndex 内部跳过，不做无谓重建。
//
// 用法（重建前建议先退出 Absorb 应用，避免 SQLite 写并发触发 busy 冲突）：
//
//	go run ./cmd/absorb-reindex
//	go run ./cmd/absorb-reindex -db "%APPDATA%\absorb-agent\data\sessions.db"
//	go run ./cmd/absorb-reindex -config config/config.yaml   # 含向量域重建
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"absorb-agent/internal/conf"
	"absorb-agent/internal/document"
	"absorb-agent/internal/retrieval"
	"absorb-agent/internal/store/storage"

	_ "absorb-agent/internal/store/sqlite" // 注册 sqlite backend 与 "sqlite" 驱动
)

// documentChunkerVersion 重建目标版本（与 document 包当前切块规则一致）。
const documentChunkerVersion = document.ChunkerVersion

func main() {
	dbPath := flag.String("db", defaultDBPath(), "sessions.db 路径（默认自动定位数据目录）")
	cfgPath := flag.String("config", "", "config.yaml 路径；提供且配置了 embedding 时同步重建向量域")
	flag.Parse()

	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "reindex: 数据库不存在: %s（用 -db 指定路径）\n", *dbPath)
		os.Exit(1)
	}
	fmt.Printf("== Absorb 存量索引重建 ==\n库: %s\n", *dbPath)
	if _, err := os.Stat(filepath.Join(filepath.Dir(*dbPath), "bridge.port")); err == nil {
		fmt.Println("⚠ 检测到应用可能正在运行（bridge.port 存在），建议先退出 Absorb 再执行")
	}

	// 可选 embedding：与 main.go setupEmbedding 同款装配（只要 embedder，
	// rerank 不参与索引期）。云 API，无子进程生命周期。
	var embedder retrieval.Embedder
	if *cfgPath != "" {
		embedder = setupEmbedder(conf.LoadConfig(*cfgPath))
	}
	if embedder != nil {
		fmt.Printf("向量域: 启用（model=%s）\n", embedder.Model())
	} else {
		fmt.Println("向量域: 跳过（未提供 -config 或未配置 embedding），下次 prepare 自动补建")
	}

	started := time.Now()
	stats, err := rebuildAll(context.Background(), *dbPath, embedder,
		func(line string) { fmt.Println(line) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "reindex: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n== 完成 | 重建 %d · 跳过(已最新) %d · 无正文 %d · 失败 %d | 耗时 %s ==\n",
		stats.rebuilt, stats.skipped, stats.noContent, stats.failed,
		time.Since(started).Round(time.Second))
	if stats.failed > 0 {
		fmt.Println("失败文档已标记 failed 保留旧索引，修复后重跑本脚本或下次 prepare 自动重试")
	}
}

// reindexStats 重建结果统计。
type reindexStats struct {
	total     int
	rebuilt   int
	skipped   int
	failed    int
	noContent int
}

// rebuildAll 全量重建入口（main 与冒烟测试共用）：枚举 documents → 逐文档
// 生产同款 BuildIndex → 三态报告。embedder 为 nil 时纯词法（向量域待下次补建）。
// 单文档失败标记 failed 不中断（索引是派生数据，见 internal/document/index.go）。
func rebuildAll(ctx context.Context, dbPath string, embedder retrieval.Embedder,
	printf func(string)) (reindexStats, error) {

	var stats reindexStats
	docs, err := listDocuments(dbPath)
	if err != nil {
		return stats, fmt.Errorf("枚举文档失败: %w", err)
	}
	stats.total = len(docs)
	printf(fmt.Sprintf("待处理文档: %d 份", len(docs)))

	st, err := storage.New("sqlite", dbPath) // 打开即前向迁移（幂等）
	if err != nil {
		return stats, fmt.Errorf("打开库失败: %w", err)
	}
	defer st.Close()

	// 按 (app,user) 归组建 Service（归属维度与问答链路同值，Service 单组持有）
	type groupKey struct{ app, user string }
	groups := map[groupKey][]docRef{}
	for _, d := range docs {
		k := groupKey{d.app, d.user}
		groups[k] = append(groups[k], d)
	}

	for k, refs := range groups {
		svc := newReindexService(st, k.app, k.user).WithEmbedder(embedder)
		for _, d := range refs {
			dc, err := st.LoadDocumentContentByID(ctx, k.app, k.user, d.id)
			if err != nil || dc == nil || strings.TrimSpace(dc.ContentText) == "" {
				printf(fmt.Sprintf("  [跳过] %s：无已提取正文", display(d)))
				stats.noContent++
				continue
			}
			before := loadState(ctx, st, k.app, k.user, d.id)
			docStart := time.Now()
			svc.BuildIndex(ctx, d.id, dc)
			after := loadState(ctx, st, k.app, k.user, d.id)
			elapsed := time.Since(docStart).Round(time.Millisecond)

			switch {
			case after == nil || after.IndexStatus == storage.IndexStatusFailed:
				printf(fmt.Sprintf("  [失败] %s：%s（%s）", display(d), stateBrief(after), elapsed))
				stats.failed++
			case sameLexical(before, after):
				printf(fmt.Sprintf("  [跳过] %s：%s 已是当前版本", display(d), stateBrief(after)))
				stats.skipped++
			default:
				printf(fmt.Sprintf("  [重建] %s：%s → %s（%s）", display(d), stateBrief(before), stateBrief(after), elapsed))
				stats.rebuilt++
			}
		}
	}
	return stats, nil
}

// newReindexService 生产同款文档服务（无提取器——重建只消费已落库正文）。
func newReindexService(st storage.Storage, app, user string) *document.Service {
	return document.NewService(st, nil, app, user)
}

// docRef documents 表行引用（枚举快照）。
type docRef struct {
	id    string
	app   string
	user  string
	title string
	url   string
}

// sqlOpen 以生产同款参数打开原始 SQLite 连接（枚举/维护用短连接，
// busy_timeout 与存储层一致）。
func sqlOpen(dbPath string) (*sql.DB, error) {
	return sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
}

// listDocuments 枚举 documents 全表锚点（只读）：文档可能无 content 行
// （prepare 失败不落正文），由调用方跳过。
func listDocuments(dbPath string) ([]docRef, error) {
	db, err := sqlOpen(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, app_name, user_id, title, url
		FROM documents ORDER BY app_name, user_id, url`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []docRef
	for rows.Next() {
		var d docRef
		if err := rows.Scan(&d.id, &d.app, &d.user, &d.title, &d.url); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// defaultDBPath 自动定位生产库：安装模式 %APPDATA%\absorb-agent\data\sessions.db
// 优先；不存在回落 ./data/sessions.db（便携/CLI 工作目录形态）。
func defaultDBPath() string {
	if base, err := os.UserConfigDir(); err == nil {
		p := filepath.Join(base, "absorb-agent", "data", "sessions.db")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join("data", "sessions.db")
}

// setupEmbedder 按配置构造索引期 Embedder（main.go setupEmbedding 的索引子集）：
// 云 = OpenAI 兼容 API；未配置 = nil（纯词法）。bge 时代 local llama-server
// sidecar 模式已随 bge 一并移除。
func setupEmbedder(cfg conf.Config) retrieval.Embedder {
	emb := cfg.Embedding
	if strings.TrimSpace(emb.APIKey) == "" {
		return nil // 未配置：纯词法
	}
	return retrieval.NewOpenAICompatEmbedder(
		emb.BaseURL, emb.APIKey, emb.Model, emb.BatchSize,
		func() string { return cfg.Proxy.URL })
}

// loadState 读索引状态（查无 = nil）；失败仅告警不中断（报告缺 before 不影响重建）。
func loadState(ctx context.Context, st storage.Storage, app, user, docID string) *storage.DocumentIndexState {
	s, err := st.LoadDocumentIndexState(ctx, app, user, docID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reindex: 读索引状态失败 (doc=%s): %v\n", docID, err)
		return nil
	}
	return s
}

// sameLexical 词法域是否已与当前版本一致（BuildIndex 幂等跳过的等价判定）。
func sameLexical(before, after *storage.DocumentIndexState) bool {
	return before != nil && after != nil &&
		before.ChunkerVersion == after.ChunkerVersion &&
		before.IndexedContentHash == after.IndexedContentHash &&
		before.IndexStatus == storage.IndexStatusReady
}

// stateBrief 状态摘要：chunker 版本/状态/块数 + 向量域。
func stateBrief(st *storage.DocumentIndexState) string {
	if st == nil {
		return "无索引"
	}
	s := fmt.Sprintf("v%d/%s/%d块", st.ChunkerVersion, st.IndexStatus, st.ChunkCount)
	if st.VectorStatus == storage.VectorStatusReady {
		s += fmt.Sprintf("/向量:ready(%d,%s)", st.VectorCount, st.VectorModel)
	} else if st.VectorStatus == storage.VectorStatusFailed {
		s += "/向量:failed"
	}
	return s
}

// display 展示名：title 优先，空则取 URL 末段。
func display(d docRef) string {
	if strings.TrimSpace(d.title) != "" {
		return d.title
	}
	name := d.url
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	return name
}
