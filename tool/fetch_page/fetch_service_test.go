package fetch

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ===== 测试样本：手工构建最小合法 PDF（零第三方依赖）=====

// buildMinimalPDF 构建单页 PDF，页面打印给定文本（仅 ASCII，避免括号转义问题）。
// 手工计算 xref 偏移，保证 dslipak/pdf 可直接解析。
func buildMinimalPDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var buf bytes.Buffer
	offsets := make([]int, len(objs))
	buf.WriteString("%PDF-1.4\n")
	for i, o := range objs {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xrefStart := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xrefStart)
	return buf.Bytes()
}

// ===== FetchPageService 直测 =====

func TestFetchService_HTML(t *testing.T) {
	srv := withBody(t, "text/html; charset=utf-8", pageFixture())
	defer srv.Close()
	svc := NewFetchPageService(func() string { return "" }, nil)
	doc, err := svc.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	for _, k := range []string{"这是标题", "这是正文内容", "第一项"} {
		if !strings.Contains(doc.Content, k) {
			t.Errorf("正文缺少 %q", k)
		}
	}
	if doc.URL != srv.URL {
		t.Errorf("URL 应为请求地址, got %q", doc.URL)
	}
	if doc.ByteSize == 0 || doc.ContentType == "" {
		t.Errorf("元信息不完整: type=%q size=%d", doc.ContentType, doc.ByteSize)
	}
}

func TestFetchService_PDF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(buildMinimalPDF("Hello PDF Service"))
	}))
	defer srv.Close()
	svc := NewFetchPageService(func() string { return "" }, nil)
	doc, err := svc.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("PDF Fetch 失败: %v", err)
	}
	if !strings.Contains(doc.Content, "Hello PDF Service") {
		t.Errorf("PDF 文本层应被提取, got: %q", doc.Content)
	}
	if !strings.Contains(doc.ContentType, "pdf") {
		t.Errorf("ContentType 应为 PDF 类型, got %q", doc.ContentType)
	}
}

func TestFetchService_HTTPError(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		svc := NewFetchPageService(func() string { return "" }, nil)
		_, err := svc.Fetch(context.Background(), srv.URL)
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d 应返回错误", code)
		}
		fe, ok := err.(*FetchError)
		if !ok {
			t.Fatalf("HTTP %d 错误应为 *FetchError, got %T", code, err)
		}
		if fe.StatusCode != code {
			t.Errorf("HTTP %d 错误码应保留, got %d", code, fe.StatusCode)
		}
	}
}

func TestFetchService_EmptyContent(t *testing.T) {
	srv := withBody(t, "text/html; charset=utf-8", "<html><body></body></html>")
	defer srv.Close()
	svc := NewFetchPageService(func() string { return "" }, nil)
	_, err := svc.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "未包含可读文本") {
		t.Fatalf("空内容应报可读错误, got: %v", err)
	}
}

func TestFetchService_EmptyURL(t *testing.T) {
	svc := NewFetchPageService(func() string { return "" }, nil)
	_, err := svc.Fetch(context.Background(), "  ")
	if err == nil || !strings.Contains(err.Error(), "URL 不能为空") {
		t.Fatalf("空 URL 应报错, got: %v", err)
	}
}

// ===== 内容上限：PDF 超限报错 / 文本静默截断（httpfetch 语义）+ 动态热生效 =====

func TestFetchService_OverLimit(t *testing.T) {
	// PDF：body 超限直接报错（readPDFAsText 硬检查）
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(bytes.Repeat([]byte("x"), 10*1024))
	}))
	defer pdfSrv.Close()
	maxBytes := int64(1024)
	svc := NewFetchPageService(func() string { return "" }, func() int64 { return maxBytes })
	if _, err := svc.Fetch(context.Background(), pdfSrv.URL); err == nil {
		t.Fatal("PDF 超限应报错")
	}

	// 文本：超限被 httpfetch 静默截断，服务层应标记 Truncated
	long := strings.Repeat("x", 10*1024)
	txtSrv := withBody(t, "text/plain; charset=utf-8", long)
	defer txtSrv.Close()
	doc, err := svc.Fetch(context.Background(), txtSrv.URL)
	if err != nil {
		t.Fatalf("文本超限应截断返回: %v", err)
	}
	if !doc.Truncated || int64(len(doc.Content)) != maxBytes {
		t.Errorf("应截断到上限并标记 Truncated, got truncated=%v len=%d", doc.Truncated, len(doc.Content))
	}

	// 上限调大后同一实例下次请求生效（无需重建服务）
	maxBytes = 1024 * 1024
	doc, err = svc.Fetch(context.Background(), txtSrv.URL)
	if err != nil {
		t.Fatalf("上调上限后应成功: %v", err)
	}
	if doc.Truncated || len(doc.Content) != len(long) {
		t.Errorf("上调后应完整返回, got truncated=%v len=%d", doc.Truncated, len(doc.Content))
	}
}

// ===== fetch_page 适配层：模型侧截断只发生在此层，服务层返回完整正文 =====

func TestFetchService_FullContentWhileToolTruncates(t *testing.T) {
	// 55000 字 > fetchMaxRunes(20000)，但 < 默认 10MB 服务上限
	long := strings.Repeat("这是一段超长正文内容。", 5500)
	srv := withBody(t, "text/plain; charset=utf-8", long)
	defer srv.Close()

	svc := NewFetchPageService(func() string { return "" }, nil)
	doc, err := svc.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("服务层应返回完整正文: %v", err)
	}
	if len([]rune(doc.Content)) <= fetchMaxRunes {
		t.Fatal("服务层正文不应被 20k 截断")
	}

	out := fetchViaURL(t, srv.URL)
	if !out.Success || !strings.Contains(out.Content, "已截断") {
		t.Fatal("工具适配层输出应被 20k 截断并标注")
	}
}
