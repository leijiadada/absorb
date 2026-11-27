// absorb-m2verify M2 跨文档检索本地验证 CLI。
//
// 用真实文档（本地 PDF/txt/md）在临时库上走「生产同款提取 → 分块分词索引 →
// FTS 检索 → 多样性融合」全链路，输出分组检索结果与观测数据。
// PDF 经本地 HTTP 服务 + 生产 PDFExtractor（httpfetch 原品提取）；
// txt/md 为纯文本直接读入（无转换必要）。
//
// 用法：
//
//	go run ./cmd/absorb-m2verify -dir "D:\docs" -query "问题一" -query "问题二"
//	go run ./cmd/absorb-m2verify -file a.pdf -file b.md -file c.txt -query "..."
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"absorb-agent/internal/document"
	"absorb-agent/internal/document/extract"
	"absorb-agent/internal/store/storage"
	fetch "absorb-agent/tool/fetch_page"
	searchdoc "absorb-agent/tool/search_document"

	_ "absorb-agent/internal/store/sqlite"
)

const (
	verifyApp  = "absorb-reader"
	verifyUser = "m2-verify"
)

func main() {
	dir := flag.String("dir", "", "扫描目录（*.pdf/*.txt/*.md，递归一层）")
	var files sliceFlag
	flag.Var(&files, "file", "单份文档路径，可重复")
	var queries sliceFlag
	flag.Var(&queries, "query", "验证查询，可重复")
	flag.Parse()

	paths := collectDocs(*dir, files)
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "未发现文档：用 -dir 或 -file 指定（支持 pdf/txt/md）")
		os.Exit(1)
	}
	if len(queries) == 0 {
		queries = []string{"这些资料的核心观点是什么"}
	}

	// 1. 临时库（跑完即弃）
	tmp, err := os.MkdirTemp("", "absorb-m2verify-*")
	must(err)
	defer os.RemoveAll(tmp)
	st, err := storage.New("sqlite", filepath.Join(tmp, "verify.db"))
	must(err)
	defer st.Close()
	ctx := context.Background()

	// 2. 文档导入：PDF 走本地 HTTP 服务 + 生产提取器；txt/md 直读
	var pdfPaths []string
	for _, p := range paths {
		if strings.EqualFold(filepath.Ext(p), ".pdf") {
			pdfPaths = append(pdfPaths, p)
		}
	}
	urlFor := servePDFs(pdfPaths)

	fmt.Printf("== Absorb M2 跨文档检索验证 ==\n文档 %d 份：\n", len(paths))
	// 内容上限放宽到 200MB：技术书 PDF 常超生产默认 10MB，本地服务无风险；
	// 代理直连、双跑择优参数与主路一致
	ex := extract.NewPDFExtractor(fetch.NewFetchPageService(func() string { return "" },
		func() int64 { return 200 << 20 }), func() string { return "" },
		func() int64 { return 200 << 20 })
	svc := document.NewService(st, ex, verifyApp, verifyUser)
	for _, p := range paths {
		start := time.Now()
		text, _ := readDoc(ctx, ex, urlFor[p], p)
		if strings.TrimSpace(text) == "" {
			fmt.Printf("  [跳过] %s（无可提取文本）\n", filepath.Base(p))
			continue
		}
		// 与 Prepare 相同的落库三步（Prepare 含 SSRF 校验，本地 file:// 由本
		// 程序自担——仅验证进程可达），DocumentID = documents.id UUID（M0 语义）。
		url := "file://" + filepath.ToSlash(p)
		doc, err := st.UpsertDocument(ctx, verifyApp, verifyUser, url, strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
		if err != nil {
			fmt.Printf("  [失败] %s: %v\n", filepath.Base(p), err)
			continue
		}
		sum := sha256.Sum256([]byte(text))
		dc := &storage.DocumentContent{
			URL: url, ContentText: text,
			CharCount: len([]rune(text)), ContentType: "application/octet-stream",
			ByteSize: int64(len(text)), ContentHash: hex.EncodeToString(sum[:]),
			ExtractionStatus: extract.StatusReady, ExtractionVersion: 1,
			FetchedAt: time.Now(),
		}
		if err := st.SaveDocumentContent(ctx, verifyApp, verifyUser, dc); err != nil {
			fmt.Printf("  [失败] %s: %v\n", filepath.Base(p), err)
			continue
		}
		svc.BuildIndex(ctx, doc.ID, dc)
		state, _ := st.LoadDocumentIndexState(ctx, verifyApp, verifyUser, doc.ID)
		status := "—"
		if state != nil {
			status = fmt.Sprintf("%s/%d chunks", state.IndexStatus, state.ChunkCount)
		}
		fmt.Printf("  [%s] %s：%d 字 → %s（%s）\n", dc.ExtractionStatus, filepath.Base(p),
			dc.CharCount, status, time.Since(start).Round(time.Millisecond))
	}

	// 3. 查询验证：全部资料范围 + M2 融合
	a := document.SharedAnalyzer()
	for _, q := range queries {
		fmt.Printf("\n── 查询：%q\n", q)
		started := time.Now()
		raw, err := st.SearchDocumentChunks(ctx, verifyApp, verifyUser,
			a.QueryExpression(q), nil, searchdoc.RawRecallLimit)
		if err != nil {
			fmt.Printf("   检索失败：%v\n", err)
			continue
		}
		final, docsCovered := searchdoc.DiversifyHits(raw, searchdoc.PerDocLimit, searchdoc.FinalLimit)
		fmt.Printf("   raw=%d final=%d docs=%d dur=%s\n",
			len(raw), len(final), docsCovered, time.Since(started).Round(time.Millisecond))
		if len(final) == 0 {
			fmt.Println("   （零命中）")
			continue
		}
		var order []string
		groups := map[string][]storage.DocumentChunkHit{}
		for _, h := range final {
			if _, ok := groups[h.DocumentID]; !ok {
				order = append(order, h.DocumentID)
			}
			groups[h.DocumentID] = append(groups[h.DocumentID], h)
		}
		for i, id := range order {
			hs := groups[id]
			fmt.Printf("   【资料 %d】%s（%d 条，bm25 %.2f）\n", i+1, hs[0].Title, len(hs), hs[0].Score)
			for _, h := range hs {
				content := []rune(strings.TrimSpace(h.Content))
				if len(content) > 80 {
					content = content[:80]
				}
				fmt.Printf("      · %s…\n", string(content))
			}
		}
	}
}

// readDoc 提取单份文档正文：PDF 经本地 HTTP（生产同款），纯文本直读。
// pdfURL 为空 = 非 PDF（直接读文件）。
func readDoc(ctx context.Context, ex extract.Extractor, pdfURL, path string) (string, string) {
	if pdfURL == "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", ""
		}
		return string(b), "text/plain"
	}
	res, err := ex.Extract(ctx, pdfURL)
	if err != nil {
		return "", ""
	}
	return res.Content, res.ContentType
}

// servePDFs 起 127.0.0.1 随机端口静态服务（仅存活于本进程）。
// ServeMux 的 pattern 不能含空格（会被当 method 解析），故文件名一律 slug 化
// （/d0.pdf），返回 真实路径 → URL 映射。
func servePDFs(files []string) map[string]string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	urlFor := make(map[string]string, len(files))
	mux := http.NewServeMux()
	for i, p := range files {
		slug := fmt.Sprintf("/d%d%s", i, strings.ToLower(filepath.Ext(p)))
		target := p // 闭包捕获
		mux.HandleFunc(slug, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, target)
		})
		urlFor[p] = "http://" + ln.Addr().String() + slug
	}
	go http.Serve(ln, mux)
	return urlFor
}

// collectDocs 汇总文档路径：-dir 扫描一层 + -file 直加，去重排序。
func collectDocs(dir string, files []string) []string {
	set := map[string]struct{}{}
	for _, f := range files {
		if isDoc(f) {
			set[filepath.Clean(f)] = struct{}{}
		}
	}
	if dir != "" {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if isDoc(p) {
				set[p] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func isDoc(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".pdf", ".txt", ".md":
		return true
	}
	return false
}

type sliceFlag []string

func (s *sliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *sliceFlag) Set(v string) error { *s = append(*s, v); return nil }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
}
