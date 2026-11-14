package retriever

// pageextract 图片占位分支的单测：与 JS pageconvert.spec.ts 的 img 用例同构。
// 覆盖：基本占位 / alt 回退链 / 懒加载 src 回退 / data-uri 与 javascript: 跳过 /
// 装饰图启发式 / 宽高属性 / 单页行数上限与省略行 / 行内位置不破坏文本流。

import (
	"strings"
	"testing"
)

func TestExtractPageTextImgBasics(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{"基本占位", `<img src="https://x.com/a.png" alt="架构图">`, "![架构图](https://x.com/a.png)"},
		{"alt缺失回退title", `<img src="https://x.com/a.png" title="流程图">`, "![流程图](https://x.com/a.png)"},
		{"alt_title缺失回退文件名", `<img src="https://x.com/img/diagram-v2.png">`, "![diagram-v2.png](https://x.com/img/diagram-v2.png)"},
		{"全空回退图片字", `<img src="https://x.com/img/">`, "![图片](https://x.com/img/)"},
		{"懒加载data-src优先", `<img src="loading.gif" data-src="https://x.com/real.png" alt="真实图">`, "![真实图](https://x.com/real.png)"},
		{"data_original回退", `<img data-original="https://x.com/lazy.png" alt="懒">`, "![懒](https://x.com/lazy.png)"},
		{"data_uri跳过", `<img src="data:image/png;base64,iVBOR" alt="内联图">`, ""},
		{"javascript伪协议跳过", `<img src="javascript:void(0)" alt="js">`, ""},
		{"无src跳过", `<img alt="无地址">`, ""},
		{"装饰图无alt跳过", `<img src="https://x.com/static/icon.png">`, ""},
		{"装饰图有alt保留", `<img src="https://x.com/static/icon.png" alt="图标说明">`, "![图标说明](https://x.com/static/icon.png)"},
		{"宽高属性进标签", `<img src="https://x.com/a.png" alt="架构图" width="1200" height="800">`, "![架构图|1200x800](https://x.com/a.png)"},
		{"宽高缺一不追加", `<img src="https://x.com/a.png" alt="架构图" width="1200">`, "![架构图](https://x.com/a.png)"},
		{"段落内行内占位不破坏文本流", `<p>前文<img src="https://x.com/a.png" alt="图">后文</p>`, "前文![图](https://x.com/a.png)后文"},
		{"无alt长文件名截断", `<img src="https://x.com/img/` + strings.Repeat("a", 80) + `.png">`,
			"![" + strings.Repeat("a", 60) + "…](https://x.com/img/" + strings.Repeat("a", 80) + ".png)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractPageText("<html><body>" + c.html + "</body></html>")
			if !strings.Contains(got, c.want) {
				t.Errorf("输出应包含 %q，实际：\n%s", c.want, got)
			}
		})
	}
}

func TestExtractPageTextImgSkipMeansAbsent(t *testing.T) {
	// 跳过类（data-uri/装饰图/无 src）不应在输出中留下任何占位痕迹
	got := ExtractPageText("<html><body><p>正文</p>" +
		`<img src="data:image/gif;base64,R0lGOD" alt="a">` +
		`<img src="https://x.com/icon.png">` +
		`<img alt="无src">` +
		"</body></html>")
	if strings.Contains(got, "![") {
		t.Errorf("跳过的图片不应产生占位，实际：\n%s", got)
	}
	if !strings.Contains(got, "正文") {
		t.Errorf("正文不应受图片跳过影响，实际：\n%s", got)
	}
}

func TestExtractPageTextImgLineCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<p>正文开头</p>")
	for i := 0; i < 25; i++ {
		sb.WriteString(`<img src="https://x.com/p.png" alt="图` + string(rune('A'+i)) + `">`)
	}
	sb.WriteString("<p>正文结尾</p>")
	got := ExtractPageText("<html><body>" + sb.String() + "</body></html>")
	if n := strings.Count(got, "!["); n != 20 {
		t.Errorf("占位行应在上限 20 处截断，实际 %d", n)
	}
	if !strings.Contains(got, "（另省略 5 张图）") {
		t.Errorf("应补省略行「（另省略 5 张图）」，实际：\n%s", got)
	}
	if !strings.Contains(got, "正文结尾") {
		t.Errorf("上限截断不应吞掉后续正文，实际：\n%s", got)
	}
}

func TestExtractPageTextImgInsideFigure(t *testing.T) {
	// figure 内 img：figure 块级换行包裹，figcaption 文字与占位行共存
	got := ExtractPageText("<html><body><figure>" +
		`<img src="https://x.com/arch.png" alt="架构图">` +
		"<figcaption>系统架构示意</figcaption></figure></body></html>")
	for _, want := range []string{"![架构图](https://x.com/arch.png)", "系统架构示意"} {
		if !strings.Contains(got, want) {
			t.Errorf("figure 内应同时保留占位与图注 %q，实际：\n%s", want, got)
		}
	}
}
