// furniture_test.go 版式家具过滤单元测试：邻接指纹规则的剔除/保留边界。
package extract

import (
	"strconv"
	"strings"
	"testing"
)

// buildPages 构造 \f 分页文本：pages[i] 为每页原始内容（含内部换行）。
func buildPages(pages []string) string {
	return strings.Join(pages, "\f")
}

// pageOf 取过滤后文本的第 i 页（按 \f 切分）。
func pageOf(t *testing.T, text string, i int) string {
	t.Helper()
	parts := strings.Split(text, "\f")
	if i >= len(parts) {
		t.Fatalf("页 %d 越界，共 %d 页", i, len(parts))
	}
	return parts[i]
}

// richPage 构造一页富内容：页首行 + 3 行逐页不同的正文 + 页尾行（≥5 非空行，
// 越过候选门控；正文逐页不同避免被指纹规则误判）。
func richPage(head, tail string, i int) string {
	return head + "\n正文甲段" + strconv.Itoa(i) + "，讨论切片底层实现。\n\n" +
		"正文乙段" + strconv.Itoa(i) + "，扩容策略与内存布局。\n\n" +
		"正文丙段" + strconv.Itoa(i) + "，边界条件与越界语义。\n\n" + tail + "\n"
}

func TestStripPDFFurniture_RemovesConsecutiveHeader(t *testing.T) {
	var pages []string
	for i := 1; i <= 6; i++ {
		pages = append(pages, richPage("深入浅出Go语言", "- "+strconv.Itoa(i)+" -", i))
	}
	out, removed := stripPDFFurniture(buildPages(pages))
	if removed != 6 {
		t.Fatalf("应剔除 6 行页眉，实际 %d", removed)
	}
	for i := 0; i < 6; i++ {
		p := pageOf(t, out, i)
		if strings.Contains(p, "深入浅出Go语言") {
			t.Fatalf("页 %d 页眉未剔除:\n%s", i, p)
		}
		if !strings.Contains(p, "正文甲段"+strconv.Itoa(i+1)) || !strings.Contains(p, "正文丙段"+strconv.Itoa(i+1)) {
			t.Fatalf("页 %d 正文被误伤:\n%s", i, p)
		}
		if !strings.Contains(p, "- "+strconv.Itoa(i+1)+" -") {
			t.Fatalf("页 %d 页码型页脚被误删:\n%s", i, p)
		}
	}
	if got := strings.Count(out, "\f"); got != 5 {
		t.Fatalf("分页符数量应保持 5，实际 %d", got)
	}
}

func TestStripPDFFurniture_ConstantFooterRemoved(t *testing.T) {
	var pages []string
	for i := 1; i <= 5; i++ {
		pages = append(pages, richPage("第"+strconv.Itoa(i)+"章 总论", "本书由某某出版社授权发布", i))
	}
	out, removed := stripPDFFurniture(buildPages(pages))
	if removed != 5 {
		t.Fatalf("应剔除 5 行页脚，实际 %d", removed)
	}
	if strings.Contains(out, "本书由某某出版社授权发布") {
		t.Fatalf("恒定页脚未剔除:\n%s", out)
	}
	for i := 1; i <= 5; i++ {
		if !strings.Contains(pageOf(t, out, i-1), "第"+strconv.Itoa(i)+"章 总论") {
			t.Fatalf("页 %d 变化页首被误删", i)
		}
	}
}

func TestStripPDFFurniture_VaryingPageNumberFooterKept(t *testing.T) {
	var pages []string
	for i := 1; i <= 6; i++ {
		pages = append(pages, richPage("深入浅出Go语言", "- "+strconv.Itoa(i)+" -", i))
	}
	out, removed := stripPDFFurniture(buildPages(pages))
	// 仅页眉被剔（6 行），页码型页脚指纹逐页不同不命中
	if removed != 6 {
		t.Fatalf("应仅剔除 6 行页眉，实际 %d", removed)
	}
	for i := 1; i <= 6; i++ {
		if !strings.Contains(pageOf(t, out, i-1), "- "+strconv.Itoa(i)+" -") {
			t.Fatalf("第 %d 页页脚被误删", i)
		}
	}
}

func TestStripPDFFurniture_BodyOccurrenceBlocksRemoval(t *testing.T) {
	var pages []string
	for i := 1; i <= 6; i++ {
		p := "敏捷宣言\n正文甲段" + strconv.Itoa(i) + "。\n\n正文乙段" + strconv.Itoa(i) + "。\n\n"
		if i == 3 {
			p += "敏捷宣言\n\n" // 中部行出现同文本 → 该指纹进入正文区
		}
		p += "正文丙段" + strconv.Itoa(i) + "。\n\n- " + strconv.Itoa(i) + " -\n"
		pages = append(pages, p)
	}
	out, removed := stripPDFFurniture(buildPages(pages))
	if removed != 0 {
		t.Fatalf("正文区出现过的指纹不应剔除，实际剔除 %d 行", removed)
	}
	if got := strings.Count(out, "敏捷宣言"); got != 7 {
		t.Fatalf("敏捷宣言应保留 7 处，实际 %d", got)
	}
}

func TestStripPDFFurniture_RunShorterThanFourKept(t *testing.T) {
	var pages []string
	for i := 1; i <= 3; i++ { // 页眉只出现在 3 个连续页（<4）
		pages = append(pages, richPage("短命页眉", "- "+strconv.Itoa(i)+" -", i))
	}
	for i := 4; i <= 6; i++ {
		pages = append(pages, richPage("无页眉页", "- "+strconv.Itoa(i)+" -", i))
	}
	out, removed := stripPDFFurniture(buildPages(pages))
	if removed != 0 {
		t.Fatalf("连续页数不足 4 不应剔除，实际剔除 %d 行", removed)
	}
	if !strings.Contains(out, "短命页眉") {
		t.Fatalf("短命页眉被误删:\n%s", out)
	}
}

func TestStripPDFFurniture_ContinuousTextUntouched(t *testing.T) {
	in := "无分页的连续文本，主路 httpfetch 产物。\n第二行正文。\n"
	out, removed := stripPDFFurniture(in)
	if removed != 0 || out != in {
		t.Fatalf("无 \\f 连续文本应原样返回: removed=%d out=%q", removed, out)
	}
}

func TestStripPDFFurniture_TinyPagesSkipped(t *testing.T) {
	// 极短页（非空行 <=4）：不贡献指纹，即使首行恒定也不剔除
	var pages []string
	for i := 1; i <= 6; i++ {
		pages = append(pages, "每页相同的首行\n各不相同的内容"+strconv.Itoa(i)+"\n")
	}
	out, removed := stripPDFFurniture(buildPages(pages))
	if removed != 0 {
		t.Fatalf("极短页不应贡献候选，实际剔除 %d 行", removed)
	}
	for i := 1; i <= 6; i++ {
		if !strings.Contains(pageOf(t, out, i-1), "每页相同的首行") {
			t.Fatalf("第 %d 页首行被误删", i)
		}
	}
}

func TestStripPDFFurniture_FullWidthFolding(t *testing.T) {
	// 页眉混用全角字符：指纹折叠后应命中同一指纹
	var pages []string
	for i := 1; i <= 5; i++ {
		pages = append(pages, richPage("Ｇｏ语言实战（第２版）", "- "+strconv.Itoa(i)+" -", i))
	}
	out, removed := stripPDFFurniture(buildPages(pages))
	if removed != 5 {
		t.Fatalf("全角页眉应折叠命中并剔除 5 行，实际 %d", removed)
	}
	if strings.Contains(out, "Ｇｏ语言实战") {
		t.Fatalf("全角页眉未剔除:\n%s", out)
	}
}
