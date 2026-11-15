package searchdoc

// 真实语料跨文档多样性验收（M2，doc2 §6.2 / §十）：真实书 + 两份真实节选
// 衍生文档构成 3 资料库，主题词查询（每份资料都大量命中）时结果必须覆盖
// 多份资料且单文档不超上限——20 万字大文档不能垄断曝光。
//
// 数据依赖与 internal/document 的单文档验收同一副本（TEMP/absorb-m1-check.db），
// 缺失时 Skip。注意本测试不能放 document 包（DiversifyHits 在本包，会成环）。
//
//	Copy-Item "$env:APPDATA\absorb-agent\data\sessions.db" "$env:TEMP\absorb-m1-check.db"
//	go test ./tool/search_document/ -run TestRealDataMultiDocDiversity -v

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"absorb-agent/internal/document"
	"absorb-agent/internal/document/extract"
	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
)

const realDBCopy = "absorb-m1-check.db"

func TestRealDataMultiDocDiversity(t *testing.T) {
	dbPath := filepath.Join(os.TempDir(), realDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过；见文件头注释", dbPath)
	}
	st, err := storage.New("sqlite", dbPath) // 打开即前向迁移；副本就地升级
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	const (
		bookURL = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"
		app     = "absorb-reader"
		user    = "absorb-user"
	)
	dc, err := st.LoadDocumentContent(ctx, app, user, bookURL)
	if err != nil || dc == nil {
		t.Skipf("副本中无目标文档正文 (err=%v)", err)
	}
	svc := document.NewService(st, nil, app, user)

	// 两份衍生节选文档：真实内容切分，与主文档主题强重叠——构造「单文档
	// 召回量碾压其余资料」的最恶劣场景
	rs := []rune(dc.ContentText)
	excerpts := []struct {
		url, title string
		seg        []rune
	}{
		{"https://library.example.com/selection-head", "全书节选·前篇", rs[:len(rs)/4]},
		{"https://library.example.com/selection-mid", "全书节选·中篇", rs[len(rs)/2 : len(rs)*3/4]},
	}
	totalDocs := 1
	book, err := st.UpsertDocument(ctx, app, user, bookURL, "AgenticSE Book CN")
	if err != nil {
		t.Fatalf("upsert book: %v", err)
	}
	svc.BuildIndex(ctx, book.ID, dc) // 幂等：hash/版本未变则跳过
	for _, e := range excerpts {
		doc, err := st.UpsertDocument(ctx, app, user, e.url, e.title)
		if err != nil {
			t.Fatalf("upsert %s: %v", e.title, err)
		}
		seg := &storage.DocumentContent{
			URL: e.url, ContentText: string(e.seg),
			CharCount: len(e.seg), ContentType: "text/plain",
			ContentHash:      hashOf(string(e.seg)),
			ExtractionStatus: extract.StatusReady, ExtractionVersion: 1,
			FetchedAt: time.Now(),
		}
		if err := st.SaveDocumentContent(ctx, app, user, seg); err != nil {
			t.Fatalf("save %s: %v", e.title, err)
		}
		svc.BuildIndex(ctx, doc.ID, seg)
		totalDocs++
	}

	// 主题词查询（三份资料都命中），全部资料范围 + M2 融合管线
	a := document.SharedAnalyzer()
	for _, q := range []string{"软件工程", "智能体", "工程实践"} {
		raw, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(q), nil, RawRecallLimit)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		final, docsCovered := DiversifyHits(raw, PerDocLimit, FinalLimit)
		t.Logf("主题查询 %q：raw=%d final=%d docs=%d/%d", q, len(raw), len(final), docsCovered, totalDocs)
		if len(raw) == 0 {
			t.Fatalf("主题词 %q 零命中，语料异常", q)
		}
		if docsCovered < 2 {
			t.Fatalf("docs=%d：多样性失效，单文档垄断（raw=%d）", docsCovered, len(raw))
		}
		perDoc := map[string]int{}
		for _, h := range final {
			perDoc[h.DocumentID]++
		}
		for docID, n := range perDoc {
			if n > PerDocLimit {
				t.Fatalf("doc=%s hits=%d 超过单文档上限 %d", docID, n, PerDocLimit)
			}
		}
		if len(final) > FinalLimit {
			t.Fatalf("final=%d 超过总量上限 %d", len(final), FinalLimit)
		}
	}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
