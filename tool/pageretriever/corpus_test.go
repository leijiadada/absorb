package retriever

// 语料库检索评估基准：docs/page-corpus 下的真实页面快照 + meta.json（转换断言 + 检索查询）。
// 作用：
//  1. 覆盖面回归——每类真实页面（新闻/博客/文档/论坛/表格/中文站/SPA 壳）的转换与检索行为
//     有客观判据，pagesearch/pageextract 任何改动都跑此基准；
//  2. 同构复验——meta.expect.mdChars 由扩展端 pageconvert.js 产出，本测试用 Go 端
//     ExtractPageText 对同一 HTML 重算并比对 rune 数，持续验证两端逐字符同构。
// 语料目录随 docs/ 不入库（见 .gitignore），缺失时跳过——本地开发资产，不入 CI 依赖。
// 语料构成与采集说明见 docs/page-corpus/README.md。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const corpusDir = "../../docs/page-corpus"

type corpusMeta struct {
	Type     string `json:"type"`
	Captured string `json:"capturedAt"`
	Expect   struct {
		NonEmpty    bool     `json:"nonEmpty"`
		MdChars     int      `json:"mdChars"`
		MustContain []string `json:"mustContain"`
	} `json:"expect"`
	Queries []struct {
		Keywords  []string `json:"keywords"`
		ExpectHit bool     `json:"expectHit"`
		Why       string   `json:"why"`
	} `json:"queries"`
}

func TestCorpusRetrieval(t *testing.T) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Skipf("语料目录不存在（docs/ 不入库），跳过： %v", err)
	}
	ran := 0
	for _, typeEntry := range entries {
		if !typeEntry.IsDir() {
			continue
		}
		typeDir := filepath.Join(corpusDir, typeEntry.Name())
		pages, err := os.ReadDir(typeDir)
		if err != nil {
			continue
		}
		for _, page := range pages {
			if !page.IsDir() && !strings.HasSuffix(page.Name(), ".html") {
				continue
			}
			var htmlPath, metaPath string
			if page.IsDir() {
				htmlPath = filepath.Join(typeDir, page.Name(), "source.html")
				metaPath = filepath.Join(typeDir, page.Name(), "meta.json")
			} else {
				// 类型目录直挂 html 的形态（如 spa-shell/source.html）：meta 为目录级 meta.json
				htmlPath = filepath.Join(typeDir, page.Name())
				metaPath = filepath.Join(typeDir, "meta.json")
			}
			if _, err := os.Stat(htmlPath); err != nil {
				continue
			}
			raw, err := os.ReadFile(htmlPath)
			if err != nil {
				t.Fatalf("%s: %v", htmlPath, err)
			}
			metaRaw, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatalf("%s: meta.json 缺失: %v", htmlPath, err)
			}
			var meta corpusMeta
			if err := json.Unmarshal(metaRaw, &meta); err != nil {
				t.Fatalf("%s: %v", metaPath, err)
			}

			t.Run(typeEntry.Name()+"/"+strings.TrimSuffix(page.Name(), ".html"), func(t *testing.T) {
				got := ExtractPageText(string(raw))

				// 围栏状态机交叉验证（真实页面）：ParseHeadings 与 bodySegments 是两套
				// 独立的状态机实现，识别的代码区间必须一致——ParseHeadings 产出的
				// 标题行不得落在 bodySegments 视角的围栏内部（否则伪标题污染 section）。
				headingStarts := map[int]bool{}
				for _, h := range ParseHeadings(got) {
					headingStarts[h.start] = true
				}
				lines := strings.Split(got, "\n")
				inCode := false
				offset := 0
				codeLines, headingInCode := 0, 0
				for _, line := range lines {
					trimmed := strings.TrimSpace(line)
					isFence := strings.HasPrefix(trimmed, "```")
					if isFence {
						inCode = !inCode
					}
					if headingStarts[offset] {
						if inCode {
							headingInCode++
						}
					}
					if inCode && !isFence {
						codeLines++
					}
					offset += len(line) + 1
				}
				if codeLines > 0 && headingInCode > 0 {
					t.Errorf("围栏状态机分歧：%d 行代码区文本被 ParseHeadings 误判为标题", headingInCode)
				}

				// 转换断言：SPA 壳类（nonEmpty=false）产出极薄；常规页非空。
				if meta.Expect.NonEmpty {
					if len([]rune(got)) < 100 {
						t.Fatalf("转换产出过薄: %d runes", len([]rune(got)))
					}
					// 同构复验：meta.mdChars 由扩展端 pageconvert.js 生成
					if len([]rune(got)) != meta.Expect.MdChars {
						t.Fatalf("与扩展端转换产出不同构: go=%d js=%d", len([]rune(got)), meta.Expect.MdChars)
					}
					for _, want := range meta.Expect.MustContain {
						if !compactContains(got, want) {
							t.Fatalf("mustContain %q 未出现在转换文本中", want)
						}
					}
				} else {
					if len([]rune(got)) > 100 {
						t.Fatalf("壳页应产出极薄文本，实际 %d runes", len([]rune(got)))
					}
				}

				// 检索断言：关键词命中/零命中信号
				for _, q := range meta.Queries {
					_, found := SearchPageText(got, q.Keywords, "")
					if found != q.ExpectHit {
						t.Fatalf("queries %v (=%s): found=%v, want %v", q.Keywords, q.Why, found, q.ExpectHit)
					}
				}
			})
			ran++
		}
	}
	if ran == 0 {
		t.Skip("语料目录为空，跳过")
	}
	t.Logf("语料检索基准完成： %d 个页面", ran)
}

// TestCorpusMetaRegenerate 语料 meta 再生成工具：转换器行为变更（如新增 img 占位
// 分支）后跑一次，用当前 Go ExtractPageText 重算各页 expect.mdChars 并回写
// meta.json。mustContain 是人工复核项，本工具不动——回写后跑 TestCorpusRetrieval，
// mustContain 失败的条目按 docs/page-corpus/README.md 人工复核。
// 默认跳过；ABSORB_REGEN_META=1 时执行。
func TestCorpusMetaRegenerate(t *testing.T) {
	if os.Getenv("ABSORB_REGEN_META") == "" {
		t.Skip("设置 ABSORB_REGEN_META=1 以再生成语料 meta 的 mdChars")
	}
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Skipf("语料目录不存在，跳过： %v", err)
	}
	updated := 0
	for _, typeEntry := range entries {
		if !typeEntry.IsDir() {
			continue
		}
		typeDir := filepath.Join(corpusDir, typeEntry.Name())
		pages, err := os.ReadDir(typeDir)
		if err != nil {
			continue
		}
		for _, page := range pages {
			if !page.IsDir() && !strings.HasSuffix(page.Name(), ".html") {
				continue
			}
			var htmlPath, metaPath string
			if page.IsDir() {
				htmlPath = filepath.Join(typeDir, page.Name(), "source.html")
				metaPath = filepath.Join(typeDir, page.Name(), "meta.json")
			} else {
				htmlPath = filepath.Join(typeDir, page.Name())
				metaPath = filepath.Join(typeDir, "meta.json")
			}
			if _, err := os.Stat(htmlPath); err != nil {
				continue
			}
			raw, err := os.ReadFile(htmlPath)
			if err != nil {
				continue
			}
			metaRaw, err := os.ReadFile(metaPath)
			if err != nil {
				continue
			}
			var meta corpusMeta
			if err := json.Unmarshal(metaRaw, &meta); err != nil {
				continue
			}
			meta.Expect.MdChars = len([]rune(ExtractPageText(string(raw))))
			out, err := json.MarshalIndent(meta, "", "  ")
			if err != nil {
				continue
			}
			if err := os.WriteFile(metaPath, append(out, '\n'), 0o644); err != nil {
				t.Errorf("回写 %s: %v", metaPath, err)
			}
			updated++
		}
	}
	t.Logf("已更新 %d 个语料 meta", updated)
}
