package document

// 真实语料检索验收（M1，doc2 §十）：对开发库中真实提取的长文档正文
// （AgenticSE_Book_CN.pdf，209,917 字）走「索引构建 → FTS 检索」全链路，
// 验收开头/中间/末尾均可达 + 真实中文/英文术语查询表现 + 性能基线。
//
// 只读开发库副本（TEMP 下的 absorb-m1-check.db，运行前手工拷贝），
// 副本会被前向迁移到 v8 并就地建索引；源库永不触碰。
// 数据缺失时 Skip（CI 无此数据），本机命令：
//
//	Copy-Item "$env:APPDATA\absorb-agent\data\sessions.db" "$env:TEMP\absorb-m1-check.db"
//	go test ./internal/document/ -run TestRealDocumentRetrievalAcceptance -v

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "absorb-agent/internal/store/sqlite"
	"absorb-agent/internal/store/storage"
)

const realDBCopy = "absorb-m1-check.db"

func TestRealDocumentRetrievalAcceptance(t *testing.T) {
	tmp := os.TempDir()
	dbPath := filepath.Join(tmp, realDBCopy)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("开发库副本不存在（%s），跳过真实验收；见文件头注释的拷贝命令", dbPath)
	}
	st, err := storage.New("sqlite", dbPath) // 打开即前向迁移到 v8（副本就地升级）
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// 1. 取真实正文（AgenticSE Book CN，20.9 万字；归属维度与开发库实际值一致）
	const (
		url  = "https://agenticse-book.github.io/pdf/AgenticSE_Book_CN.pdf"
		app  = "absorb-reader"
		user = "absorb-user"
	)
	dc, err := st.LoadDocumentContent(ctx, app, user, url)
	if err != nil || dc == nil {
		t.Skipf("副本中无目标文档正文 (err=%v)——副本过旧？", err)
	}
	doc, err := st.UpsertDocument(ctx, app, user, url, "AgenticSE Book CN")
	if err != nil {
		t.Fatalf("upsert anchor: %v", err)
	}
	t.Logf("真实正文：%d 字，content_hash=%s", dc.CharCount, shortHash(dc.ContentHash))

	// 2. 真实内容建索引（含 GSE 分词），计时
	svc := NewService(st, nil, app, user)
	start := time.Now()
	svc.BuildIndex(ctx, doc.ID, dc)
	state, err := st.LoadDocumentIndexState(ctx, app, user, doc.ID)
	if err != nil || state == nil {
		t.Fatalf("index state: (%v, %v)", state, err)
	}
	if state.IndexStatus != storage.IndexStatusReady {
		t.Fatalf("index build failed: %+v", state)
	}
	buildDur := time.Since(start)
	t.Logf("索引构建：chunks=%d 耗时=%s", state.ChunkCount, buildDur.Round(time.Millisecond))
	if state.ChunkCount < 100 {
		t.Fatalf("chunks=%d，20.9 万字应切出 ≥100 块", state.ChunkCount)
	}

	// 3. 位置可达性：开头/中间/末尾各取一句真实文本作为检索目标，
	//    用其前若干字构造查询，断言命中片段包含原句。
	rs := []rune(dc.ContentText)
	positions := []struct {
		name string
		at   int
	}{
		{"开头", 1000},
		{"中间", len(rs) / 2},
		{"末尾", len(rs) - 2000},
	}
	a := SharedAnalyzer()
	for _, p := range positions {
		target := pickSentence(rs, p.at)
		if target == "" {
			t.Fatalf("%s位置未取到目标句", p.name)
		}
		query := queryOf(target)
		qs := time.Now()
		hits, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(query), []string{doc.ID}, 20)
		if err != nil {
			t.Fatalf("%s检索: %v", p.name, err)
		}
		found := false
		for _, h := range hits {
			if strings.Contains(h.Content, target) {
				found = true
				break
			}
		}
		t.Logf("%s 位置（offset≈%d）：query=%q 命中=%d 耗时=%s 含目标句=%v",
			p.name, p.at, query, len(hits), time.Since(qs).Round(time.Millisecond), found)
		if !found {
			t.Fatalf("%s位置目标句不可达：query=%q target=%q", p.name, query, target)
		}
	}

	// 4. 真实术语查询（信息性：命中数入日志，不做硬断言——书是否讨论该词未知）
	for _, q := range []string{"软件工程", "智能体", "AI 如何改变软件研发"} {
		qs := time.Now()
		hits, err := st.SearchDocumentChunks(ctx, app, user, a.QueryExpression(q), nil, 10)
		if err != nil {
			t.Logf("术语查询 %q 出错: %v", q, err)
			continue
		}
		t.Logf("术语查询 %q：命中=%d 耗时=%s", q, len(hits), time.Since(qs).Round(time.Millisecond))
	}
}

// pickSentence 从指定偏移向后取一句完整文本（到句末标点，最多 80 rune）。
func pickSentence(rs []rune, at int) string {
	if at < 0 || at >= len(rs) {
		return ""
	}
	end := at
	for end < len(rs) && end-at < 80 {
		r := rs[end]
		if r == '。' || r == '！' || r == '？' || r == '\n' {
			if end > at {
				end++ // 含句末标点
				break
			}
		}
		end++
	}
	return strings.TrimSpace(string(rs[at:end]))
}

// queryOf 取目标句的一个查询短语：优先取句中 6~10 字（避开开头，开头常是过渡词）。
func queryOf(sentence string) string {
	rs := []rune(sentence)
	n := len(rs)
	if n <= 10 {
		return sentence
	}
	lo, hi := 3, 13
	if hi > n {
		hi = n
	}
	return string(rs[lo:hi])
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
