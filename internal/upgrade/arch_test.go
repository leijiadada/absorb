package upgrade

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestNoInternalOrThirdPartyDependencies 架构断言：升级模块的解耦靠测试兜底，不靠约定。
//   - 禁止 import 任何 absorb-agent 内部包（chat/store/bridge/conf 一律经接口注入）；
//   - 禁止引入第三方依赖（白名单为空，新增依赖必须显式登记并说明理由）。
func TestNoInternalOrThirdPartyDependencies(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go") // 只看非测试源码
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no source files parsed")
	}

	allowedThirdParty := map[string]bool{}

	for _, pkg := range pkgs {
		for fileName, file := range pkg.Files {
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				switch {
				case strings.HasPrefix(path, "absorb-agent/"):
					t.Errorf("%s: 禁止依赖内部业务包 %s；需要的能力必须定义为接口并由 wiring 层注入", fileName, path)
				case isThirdParty(path):
					if !allowedThirdParty[path] {
						t.Errorf("%s: 未授权的第三方依赖 %s；如需引入请在白名单登记理由", fileName, path)
					}
				}
			}
		}
	}
}

// isThirdParty 判断是否为标准库之外的 import（首段含 "." 即为域名）。
func isThirdParty(path string) bool {
	first := path
	if i := strings.IndexByte(path, '/'); i >= 0 {
		first = path[:i]
	}
	return strings.Contains(first, ".")
}
