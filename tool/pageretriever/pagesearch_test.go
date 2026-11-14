package retriever

import (
	"os"
	"strings"
	"testing"
)

// mustExtract 读本包 testdata fixture 并抽取结构化文本（各测试共享）
func mustExtract(t *testing.T, fixture string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out := ExtractPageText(string(b))
	if out == "" {
		t.Fatal("ExtractPageText returned empty")
	}
	return out
}

func TestExtractPageText_Structure(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")

	// 首行页面标题
	if !strings.HasPrefix(pt, "页面标题: tRPC-Agent-Go 使用指南") {
		t.Errorf("title header missing, got head: %q", firstN(pt, 80))
	}
	// 标题层级标记：H3 带 ###，H2 带 ##
	for _, want := range []string{
		"# 快速开始",
		"## 3. 多 Agent Runner",
		"### 3.1 Runner 的定义",
		"## 4. 执行模型",
	} {
		if !strings.Contains(pt, "\n"+want+"\n") {
			t.Errorf("heading %q not found in page text", want)
		}
	}
	// 代码块围栏（PRE → ```）
	if !strings.Contains(pt, "```\nconst r = runner.NewRunner(cfg)\n```") {
		t.Errorf("code fence missing, got: %q", firstN(pt, 400))
	}
	// 正文行存在且空白折叠（script 内容不进入）
	if !strings.Contains(pt, "Runner 是 tRPC-Agent-Go 的执行载体") {
		t.Errorf("body paragraph missing")
	}
	if strings.Contains(pt, "go get") == false {
		t.Errorf("normal paragraph missing")
	}
	if strings.Contains(pt, "构造约") || strings.Contains(pt, "serverSide") {
		// doc-page 无 script 正文，此断言防御性保留
		t.Errorf("script content leaked into page text")
	}
	// 空输入
	if ExtractPageText("") != "" {
		t.Errorf("empty input should return empty")
	}
	if ExtractPageText("   \n  ") != "" {
		t.Errorf("whitespace-only input should return empty")
	}
}

// 畸形 HTML（不可信输入容错）：不 panic、能抽出可见文本
func TestExtractPageText_MalformedHTML(t *testing.T) {
	out := ExtractPageText("<div><p>未闭合段落<h2>标题</h2>内容</div>")
	if !strings.Contains(out, "未闭合段落") || !strings.Contains(out, "标题") {
		t.Errorf("malformed html extraction failed: %q", out)
	}
	// 只有未闭合 script 的畸形输入：不应 panic
	ExtractPageText(`<html><body><script>var x = '";`)
}

// 超长输入：500K 截断不 panic；结构化文本上限 30K
func TestExtractPageText_SizeCaps(t *testing.T) {
	// 构造超长正文（约 60K 字符段落文本）
	long := "<html><head><title>Big</title></head><body><h1>大文档</h1><h2>独特标记详解</h2><p>" +
		strings.Repeat("这是一段用于测试超长截断的正文内容。", 2000) +
		"独特标记出现在段落尾部。" +
		"</p><h2>下一章</h2><p>后续章节内容。</p></body></html>"
	pt := ExtractPageText(long)
	if pt == "" {
		t.Fatal("extract returned empty for long input")
	}
	// 30K 上限（rune 计）
	if len([]rune(pt)) > PageTextMaxLen+50 {
		t.Errorf("page text exceeds cap: %d runes", len([]rune(pt)))
	}
	// 截断发生在尾部：后续章节应被截掉
	if strings.Contains(pt, "后续章节内容") {
		t.Errorf("content beyond cap should be truncated")
	}
}

func TestCompactIndex(t *testing.T) {
	// 大小写 + 空白不敏感
	if i := compactIndex("3. 多 Agent Runner 是执行载体", "多agent runner"); i < 0 {
		t.Errorf("compact match should hit, got -1")
	} else {
		if !strings.Contains("3. 多 Agent Runner 是执行载体"[i:i+20], "Agent") {
			// 命中点应指向原文「多 Agent Runner」附近（字节下标回映射）
			t.Logf("hit index=%d", i)
		}
	}
	if compactIndex("abcdef", "xyz") >= 0 {
		t.Errorf("no match expected")
	}
	if compactIndex("abc", "") >= 0 {
		t.Errorf("empty needle should not match")
	}
	if compactIndex("", "abc") >= 0 {
		t.Errorf("empty haystack should not match")
	}
}

func TestSearchPageText_Stage1Heading(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")

	// 关键词"Runner"命中多个标题（## 3. 多 Agent Runner 与 ### 3.1 Runner 的定义），
	// 全部收集，最深优先，父节因包含子节内容被去重跳过。
	// 同时正文段落中其他含"Runner"的节（如 3.2 配置项、3.3 Executor）也一并收集。
	hit, found := SearchPageText(pt, []string{"Runner"}, "")
	if !found {
		t.Fatal("should hit")
	}
	if !strings.Contains(hit, "命中标题「3.1 Runner 的定义」") {
		t.Errorf("should have deepest heading 3.1, got: %q", firstN(hit, 120))
	}
	// section 内容到下一个同级标题（3.2）为止，不含 3.2 配置项
	if !strings.Contains(hit, "Runner 是 tRPC-Agent-Go 的执行载体") {
		t.Errorf("section body missing")
	}
	// 多条目应合并返回（用 --- 分隔），包含正文段落命中
	if !strings.Contains(hit, "---") {
		t.Errorf("multiple hits should be joined with separator, got: %q", firstN(hit, 300))
	}
	if !strings.Contains(hit, "配置项") {
		t.Errorf("body hits from other sections should also be collected, missing '配置项'")
	}
}

func TestSearchPageText_Stage1CompactMatch(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")
	// compact 匹配：大小写 + 空白差异（「多 Agent Runner」vs「多Agent Runner」）
	hit, found := SearchPageText(pt, []string{"多Agent runner"}, "")
	if !found || !strings.Contains(hit, "命中标题「3. 多 Agent Runner」") {
		t.Errorf("compact heading match failed, found=%v, got: %q", found, firstN(hit, 120))
	}
}

func TestSearchPageText_Stage2Body(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")
	// 「调度器」无标题命中，正文段落命中（5. 编排模式 & 6. 可观测性 都有，全部收集）
	// 参考材料 = 划选 4. 执行模型 的句子 → 与划选重叠的段落跳过
	refMat := "调度器会在每个 tick 重新计算优先级，保证高优任务先执行。"
	hit, found := SearchPageText(pt, []string{"调度器"}, refMat)
	if !found {
		t.Fatal("should hit body")
	}
	if !strings.Contains(hit, "命中段落") {
		t.Errorf("should be body hit, got: %q", firstN(hit, 120))
	}
	// 命中的应是观测性段落而非划选句所在段落（去重生效）
	if !strings.Contains(hit, "OpenTelemetry") {
		t.Errorf("should hit non-overlapping paragraph, got: %q", firstN(hit, 200))
	}
}

func TestSearchPageText_ZeroHit(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")
	// 页面没有的概念 → 零命中
	hit, found := SearchPageText(pt, []string{"LangGraph"}, "")
	if found {
		t.Fatalf("should not hit, got: %q", firstN(hit, 120))
	}
	sig := ZeroHitSignal([]string{"LangGraph"})
	if !strings.Contains(sig, "LangGraph") || !strings.Contains(sig, "不要再调用") {
		t.Errorf("zero-hit signal malformed: %q", sig)
	}
	// 多关键词任一命中即可
	if _, found := SearchPageText(pt, []string{"LangGraph", "Executor"}, ""); !found {
		t.Errorf("any-keyword match should hit Executor")
	}
}

func TestSearchPageText_Truncation(t *testing.T) {
	// 构造超长 section（>3500），验证截断取关键词附近窗口
	long := "# 大文档\n\n## 独特标记详解\n\n" + strings.Repeat("这是超长章节的正文填充内容，用于验证截断逻辑。", 300) +
		"独特标记出现在中部。" + strings.Repeat("章节尾部继续填充内容。", 300) + "\n\n## 下一章\n\n后续章节内容。"
	hit, found := SearchPageText(long, []string{"独特标记"}, "")
	if !found {
		t.Fatal("should hit heading")
	}
	// 多条目合并后总大小超过单个截断上限，但关键词上下文应保留
	if !strings.Contains(hit, "独特标记出现在中部") {
		t.Errorf("truncation should keep keyword window, got: %q…", firstN(hit, 200))
	}
	// 多条目合并返回（用 --- 分隔）
	if !strings.Contains(hit, "---") {
		t.Errorf("multiple hits should be joined with separator, got: %q", firstN(hit, 200))
	}
}

func TestRefMatWindow(t *testing.T) {
	pt := mustExtract(t, "doc-page.html")
	// 划选 3.1 定义句 → 邻近窗口覆盖前后内容
	w := RefMatWindow(pt, "Runner 是 tRPC-Agent-Go 的执行载体")
	if w == "" {
		t.Fatal("window should not be empty")
	}
	if !strings.Contains(w, "Runner 是 tRPC-Agent-Go") {
		t.Errorf("window should contain reference material")
	}
	// 定位失败退化：返回前 4000
	w2 := RefMatWindow(pt, "完全不存在于页面的参考材料文本")
	if !strings.HasPrefix(w2, "页面标题") {
		t.Errorf("fallback should return page head, got: %q", firstN(w2, 60))
	}
	// 空页面
	if RefMatWindow("", "x") != "" {
		t.Errorf("empty page should return empty")
	}
}

func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// TestParseHeadings_FenceStateMachine 代码块内的 # 行（shell/python 注释）是内容
// 不是标题：围栏状态机翻转后再解析，伪标题不得进入 headingLine 列表（否则
// sectionFrom 以伪标题为起点切碎真实章节）。
func TestParseHeadings_FenceStateMachine(t *testing.T) {
	pt := "## 真实标题\n\n```bash\n# 这是注释不是标题\nnpm install\n```\n\n正文段落。\n\n## 第二标题\n"
	hs := ParseHeadings(pt)
	if len(hs) != 2 {
		t.Fatalf("应只识别 2 个真实标题，实际 %d：", len(hs))
		for _, h := range hs {
			t.Errorf("  %q", h.text)
		}
	}
	if hs[0].text != "## 真实标题" || hs[1].text != "## 第二标题" {
		t.Fatalf("标题错误: %q, %q", hs[0].text, hs[1].text)
	}
	// 未闭合围栏：其后所有 # 行均不算标题（安全降级）
	pt2 := "```python\n# 注释\n## 伪标题\n"
	if hs2 := ParseHeadings(pt2); len(hs2) != 0 {
		t.Fatalf("未闭合围栏内不应识别标题，实际 %d 个", len(hs2))
	}
	// 围栏关闭后恢复解析
	pt3 := "```bash\n# c\n```\n\n## 恢复标题\n"
	if hs3 := ParseHeadings(pt3); len(hs3) != 1 || hs3[0].text != "## 恢复标题" {
		t.Fatalf("围栏关闭后应恢复标题解析，实际 %d 个", len(hs3))
	}
}

// TestSearchPageText_FenceFakeHeadingNotSection 伪标题不得成为 section 切分点：
// 关键词命中真实标题时，section 不应被代码块内的 # 行截断。
func TestSearchPageText_FenceFakeHeadingNotSection(t *testing.T) {
	pt := "## 部署指南\n\n先安装依赖：\n\n```bash\n# 拉取代码\ngit clone repo\n```\n\n然后配置环境变量并启动服务。\n\n## 回滚指南\n\n回滚步骤在此。\n"
	// "部署指南" 的 section 应延伸到 "回滚指南" 之前，包含完整代码块与后续正文
	out, found := SearchPageText(pt, []string{"部署指南"}, "")
	if !found {
		t.Fatal("应命中标题")
	}
	if !strings.Contains(out, "启动服务") {
		t.Errorf("section 被伪标题提前截断，丢失后续正文: %s", firstN(out, 120))
	}
	if strings.Contains(out, "回滚步骤") {
		t.Errorf("section 不应越过同级标题: %s", firstN(out, 120))
	}
}

// TestExtractPageText_OrderedList 有序列表序号：浏览器渲染的 "1. " 不在 DOM 文本中，
// 转换时补回；start 属性生效；嵌套 ol 独立计数。
func TestExtractPageText_OrderedList(t *testing.T) {
	html := `<html><body><h1>步骤</h1><ol><li>下载</li><li>安装</li><li>启动</li></ol>` +
		`<ol start="5"><li>五号</li></ol>` +
		`<ol><li>外层<ol><li>内层甲</li><li>内层乙</li></ol></li><li>外层二</li></ol></body></html>`
	pt := ExtractPageText(html)
	for _, want := range []string{"1. 下载", "2. 安装", "3. 启动", "5. 五号", "1. 外层", "1. 内层甲", "2. 内层乙", "2. 外层二"} {
		if !strings.Contains(pt, want) {
			t.Errorf("缺少有序序号 %q，输出: %s", want, firstN(pt, 120))
		}
	}
	// 无序 ul 不受影响
	htmlUl := `<html><body><ul><li>甲项</li><li>乙项</li></ul></body></html>`
	if ptUl := ExtractPageText(htmlUl); strings.Contains(ptUl, "1. ") {
		t.Errorf("ul 不应产生序号: %s", ptUl)
	}
}

// TestExtractPageText_PipeTable 表格管道化：行列关系保留（决策 A1b/A2a/A3a/A4a）。
func TestExtractPageText_PipeTable(t *testing.T) {
	// 标准表 + thead：管道行 + 分隔行
	html := `<html><body><table><thead><tr><th>功能</th><th>负责人</th></tr></thead><tbody><tr><td>载入地图</td><td>Kovarex</td></tr></tbody></table></body></html>`
	pt := ExtractPageText(html)
	want := "| 功能 | 负责人 |\n| --- | --- |\n| 载入地图 | Kovarex |"
	if !strings.Contains(pt, want) {
		t.Errorf("标准管道表缺失，got: %s", firstN(pt, 160))
	}

	// caption 独立行（A2a）
	html = `<html><body><table><caption>开发进度表</caption><tr><th>功能</th></tr><tr><td>载入地图</td></tr></table></body></html>`
	pt = ExtractPageText(html)
	if !strings.Contains(pt, "开发进度表\n\n| 功能 |") {
		t.Errorf("caption 应为表格前独立行，got: %s", firstN(pt, 160))
	}

	// rowspan 复制填充（A1b）+ colspan 占位
	html = `<html><body><table><tr><th>组</th><th>项</th></tr>` +
		`<tr><td rowspan="2">共享组</td><td>项1</td></tr><tr><td>项2</td></tr>` +
		`<tr><td colspan="2">整行</td></tr></table></body></html>`
	pt = ExtractPageText(html)
	for _, want := range []string{"| 共享组 | 项1 |", "| 共享组 | 项2 |", "| 整行 |"} {
		if !strings.Contains(pt, want) {
			t.Errorf("合并单元格处理缺失 %q，got: %s", want, firstN(pt, 220))
		}
	}

	// 单元格 | 移除（C15）：结构安全且与紧凑匹配一致
	html = `<html><body><table><tr><th>表达式</th></tr><tr><td>a || b</td></tr></table></body></html>`
	pt = ExtractPageText(html)
	if !strings.Contains(pt, "| a  b |") {
		t.Errorf("单元格 | 应移除，got: %s", firstN(pt, 160))
	}
	if strings.Contains(pt, "a || b") {
		t.Errorf("单元格内 | 未移除: %s", firstN(pt, 160))
	}

	// 布局表格回退（A4a）：td 装 p 块级段落 → 散行（非管道）
	html = `<html><body><table><tr><td><p>布局段落一。</p></td><td><p>布局段落二。</p></td></tr></table></body></html>`
	pt = ExtractPageText(html)
	if strings.Contains(pt, "| ") {
		t.Errorf("布局表格应回退散行，got: %s", firstN(pt, 160))
	}
	if !strings.Contains(pt, "布局段落一。") || !strings.Contains(pt, "布局段落二。") {
		t.Errorf("布局表格内容不应丢失: %s", firstN(pt, 160))
	}

	// 检索跨列连续匹配：管道符被 compact 忽略（忽略 | 的核心收益）
	html = `<html><body><table><tr><th>功能</th><th>负责人</th></tr><tr><td>载入地图</td><td>Kovarex</td></tr></table></body></html>`
	pt = ExtractPageText(html)
	if _, found := SearchPageText(pt, []string{"载入地图Kovarex"}, ""); !found {
		t.Errorf("跨列紧凑匹配应命中（| 被忽略）")
	}
}
