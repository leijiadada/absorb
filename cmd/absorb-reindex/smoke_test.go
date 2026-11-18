package main

// 冒烟验证（仅本机跑，不进常规 CI）：临时库种子 3 份文档（含 chunker v2 旧索引、
// 无正文锚点、纯新文档），跑 rebuildAll 验证 重建/跳过/无正文 三态与幂等重跑。
//
//	go test ./cmd/absorb-reindex/ -run TestReindexSmoke -v

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"absorb-agent/internal/store/storage"

	_ "absorb-agent/internal/store/sqlite"
)

func TestReindexSmoke(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	st, err := storage.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close() // Windows 下必须先关连接，TempDir 清理才能删掉 db 文件
	ctx := context.Background()

	const (
		app  = "absorb-reader"
		user = "smoke-user"
	)
	// 文档 1：模拟 chunker v2 旧索引（hash/版本都对不上，应被重建）
	url1 := "https://example.com/old"
	doc1, err := st.UpsertDocument(ctx, app, user, url1, "旧版本文档")
	if err != nil {
		t.Fatalf("upsert doc1: %v", err)
	}
	text1 := strings.Repeat("智能体工程改变软件研发模式。第二句用来撑长度。\n\n", 200)
	if err := saveContent(t, st, app, user, url1, text1); err != nil {
		t.Fatalf("save doc1: %v", err)
	}
	svc := newReindexService(st, app, user)
	svc.BuildIndex(ctx, doc1.ID, mustContent(t, st, app, user, url1))
	// 手工把状态改回 v2（模拟存量旧索引）
	mustDowngrade(t, dbPath, doc1.ID)

	// 文档 2：只有锚点没有正文（应跳过）
	if _, err := st.UpsertDocument(ctx, app, user, "https://example.com/empty", "无正文文档"); err != nil {
		t.Fatalf("upsert doc2: %v", err)
	}

	// 文档 3：新文本，先建好当前版本索引（重跑应跳过）
	url3 := "https://example.com/fresh"
	doc3, err := st.UpsertDocument(ctx, app, user, url3, "新版本文档")
	if err != nil {
		t.Fatalf("upsert doc3: %v", err)
	}
	text3 := strings.Repeat("检索增强生成依赖文档分块与向量召回。", 200)
	if err := saveContent(t, st, app, user, url3, text3); err != nil {
		t.Fatalf("save doc3: %v", err)
	}
	svc.BuildIndex(ctx, doc3.ID, mustContent(t, st, app, user, url3))

	// ---- 第一轮：旧版本文档应重建，其余跳过 ----
	n1, err := rebuildAll(ctx, dbPath, nil, printLine(t))
	if err != nil {
		t.Fatalf("round1: %v", err)
	}
	if n1.rebuilt != 1 || n1.skipped != 1 || n1.noContent != 1 {
		t.Fatalf("round1 = %+v, want rebuilt=1 skipped=1 noContent=1", n1)
	}
	// 重建后版本应为当前 ChunkerVersion
	st1 := loadStateForTest(t, st, app, user, doc1.ID)
	if st1.ChunkerVersion != documentChunkerVersion {
		t.Fatalf("doc1 chunker version = %d, want %d", st1.ChunkerVersion, documentChunkerVersion)
	}
	if st1.IndexStatus != "ready" || st1.ChunkCount < 5 {
		t.Fatalf("doc1 state = %s", stateBrief(st1))
	}

	// ---- 第二轮：全部跳过（幂等）----
	n2, err := rebuildAll(ctx, dbPath, nil, printLine(t))
	if err != nil {
		t.Fatalf("round2: %v", err)
	}
	if n2.rebuilt != 0 || n2.skipped != 2 || n2.noContent != 1 {
		t.Fatalf("round2 = %+v, want rebuilt=0 skipped=2 noContent=1", n2)
	}
}

// ---- 种子/断言辅助 ----

func saveContent(t *testing.T, st storage.Storage, app, user, url, text string) error {
	t.Helper()
	sum := sha256.Sum256([]byte(text))
	return st.SaveDocumentContent(context.Background(), app, user, &storage.DocumentContent{
		URL: url, ContentText: text,
		CharCount: len([]rune(text)), ContentType: "text/html",
		ByteSize: int64(len(text)), ContentHash: hex.EncodeToString(sum[:]),
		ExtractionStatus: "ready", ExtractionVersion: 1,
		FetchedAt: time.Now(),
	})
}

func mustContent(t *testing.T, st storage.Storage, app, user, url string) *storage.DocumentContent {
	t.Helper()
	dc, err := st.LoadDocumentContent(context.Background(), app, user, url)
	if err != nil || dc == nil {
		t.Fatalf("load content %s: (%v, %v)", url, dc, err)
	}
	return dc
}

func mustDowngrade(t *testing.T, dbPath, docID string) {
	t.Helper()
	db, err := sqlOpen(dbPath)
	if err != nil {
		t.Fatalf("downgrade open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE document_index_state SET chunker_version=2 WHERE document_id=?`, docID); err != nil {
		t.Fatalf("downgrade exec: %v", err)
	}
}

func loadStateForTest(t *testing.T, st storage.Storage, app, user, docID string) *storage.DocumentIndexState {
	t.Helper()
	s, err := st.LoadDocumentIndexState(context.Background(), app, user, docID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return s
}

func printLine(t *testing.T) func(string) {
	return func(line string) { fmt.Fprintln(os.Stderr, line) }
}
