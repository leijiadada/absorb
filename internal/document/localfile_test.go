package document

// 本地 file:// PDF 支持的单测：URL→路径转换、validateFileURL 白名单、
// Prepare 的 file 分流（拒绝矩阵 + 捆绑 pdftotext 探测 + 真样书成功路径）。

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"absorb-agent/internal/document/extract"
)

// toFileURL 本地路径 → 标准三斜杠 file URL（file:///D:/x/y.pdf）。
func toFileURL(t *testing.T, p string) string {
	t.Helper()
	sl := filepath.ToSlash(p)
	if !strings.HasPrefix(sl, "/") {
		sl = "/" + sl // Windows 盘符路径补前导 /
	}
	return "file://" + sl
}

func TestLocalFilePath(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 期望值按 GOOS 分形态；wantErr 非空时断言拒绝
	}{
		{"Windows 盘符", "file:///D:/lj/k8s1.pdf", "D:\\lj\\k8s1.pdf"},
		{"percent-decode 中文目录", "file:///D:/lj/%E7%94%B5%E5%AD%90%E4%B9%A6/k8s1.pdf", "D:\\lj\\电子书\\k8s1.pdf"},
		{"UNC 拒绝", "file://server/share/x.pdf", ""},
		{"非 file scheme 拒绝", "https://example.com/x.pdf", ""},
		{"缺路径拒绝", "file:///", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extract.LocalFilePath(tc.raw)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("期望拒绝，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("LocalFilePath: %v", err)
			}
			// 盘符大小写与路径分隔符的期望形态仅在 Windows 下成立
			if runtime.GOOS == "windows" && got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if got == "" {
				t.Fatalf("路径为空")
			}
		})
	}
}

func TestValidateFileURL(t *testing.T) {
	books, _ := filepath.Glob(filepath.Join("..", "..", "docs", "pdf", "*.pdf"))
	if len(books) == 0 {
		t.Skip("仓库无 PDF 样书（docs/pdf），跳过")
	}
	abs, err := filepath.Abs(books[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFileURL(toFileURL(t, abs)); err != nil {
		t.Fatalf("真实样书应通过校验: %v", err)
	}
	if err := validateFileURL("file://server/share/book.pdf"); err == nil {
		t.Fatalf("UNC 路径应拒绝")
	}
	if err := validateFileURL(toFileURL(t, "不存在目录/book.txt")); err == nil {
		// .txt 先被后缀拒绝；改为构造存在的非 pdf 文件
		tmp := filepath.Join(t.TempDir(), "book.txt")
		if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateFileURL(toFileURL(t, tmp)); err == nil || !strings.Contains(err.Error(), ".pdf") {
			t.Fatalf("非 .pdf 应拒绝并提示后缀限制, got %v", err)
		}
	}
	if err := validateFileURL(toFileURL(t, filepath.Join(t.TempDir(), "missing.pdf"))); err == nil {
		t.Fatalf("不存在的文件应拒绝")
	}
	if err := validateFileURL(toFileURL(t, t.TempDir())); err == nil {
		t.Fatalf("目录应拒绝")
	}
}

// filePrepareFixture 组装最小 prepare 环境（sqlite 临时库 + 替身提取器）。
func filePrepareFixture(t *testing.T, ex extract.Extractor) *Service {
	t.Helper()
	return NewService(newTestStore(t), ex, "app", "user")
}

func TestPrepareLocalFileRejected(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"非 pdf 后缀", "file:///D:/x/book.txt", ".pdf"},
		{"UNC", "file://server/share/book.pdf", "网络共享"},
		{"不存在", "file:///D:/missing-dir-xyz/book.pdf", "不可访问"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := &fakeExtractor{res: &extract.Result{Content: "不应被调用"}}
			res, err := filePrepareFixture(t, fx).Prepare(context.Background(), tc.url, "")
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if res.Status != extract.StatusInvalidURL {
				t.Fatalf("status=%s want invalid_url", res.Status)
			}
			if !strings.Contains(res.Message, tc.want) {
				t.Fatalf("message=%q want 含 %q", res.Message, tc.want)
			}
			if fx.called != 0 {
				t.Fatalf("远程提取器不应被调用")
			}
		})
	}
}

func TestPrepareLocalFileExtractorUnsupported(t *testing.T) {
	// 存在的合法 pdf 路径 + 提取器未实现 ExtractLocal → 明确报不支持
	tmp := filepath.Join(t.TempDir(), "book.pdf")
	if err := os.WriteFile(tmp, []byte("%PDF-1.4 fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := filePrepareFixture(t, &fakeExtractor{}).Prepare(context.Background(), toFileURL(t, tmp), "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Status != extract.StatusInvalidURL || !strings.Contains(res.Message, "不支持本地文件") {
		t.Fatalf("status=%s message=%q", res.Status, res.Message)
	}
}

func TestPrepareLocalFileReady(t *testing.T) {
	books, _ := filepath.Glob(filepath.Join("..", "..", "docs", "pdf", "*.pdf"))
	if len(books) == 0 {
		t.Skip("仓库无 PDF 样书，跳过")
	}
	if !extract.PdftotextAvailable() {
		t.Skip("pdftotext 不可用（未装 poppler 且无捆绑运行时），跳过成功路径")
	}
	abs, err := filepath.Abs(books[0])
	if err != nil {
		t.Fatal(err)
	}
	ex := extract.NewPDFExtractor(nil, nil, nil) // ExtractLocal 不经 Fetch，nil 即可
	res, err := filePrepareFixture(t, ex).Prepare(context.Background(), toFileURL(t, abs), "样书")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Status != extract.StatusReady {
		t.Fatalf("status=%s message=%q want ready", res.Status, res.Message)
	}
	if res.ContentType != "application/pdf" {
		t.Fatalf("contentType=%q want application/pdf", res.ContentType)
	}
	if res.DocumentID == "" || res.DocumentID == abs {
		t.Fatalf("documentId 应为锚点 UUID 而非 URL: %q", res.DocumentID)
	}
	if res.CharCount <= 0 || res.ByteSize <= 0 {
		t.Fatalf("chars=%d bytes=%d 应有实际提取量", res.CharCount, res.ByteSize)
	}
}
