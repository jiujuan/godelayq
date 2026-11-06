package sqlite

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本测试钉死 store/sqlite 作为公共底座的一条红线（doc.go）：本包对仓内业务包
// （godelayq/core、godelayq/api、godelayq/executor…）零依赖。机制包一旦反过来
// 依赖某个业务域，其它域再想复用就得连着业务一起拖进来，"公共"二字随即失效。
//
// 实现走 go/parser 而不是编译期检查：import 违规发生在任何测试运行之前，
// 用例必须能在那个时点就把问题指出来，而不是让编译器给一条不带解释的报错。
// 与 config_reload_test.go 的 TestEveryLeafKeyIsClassed 同一取向：清单靠不住时，
// 守卫替人记住。
func TestSQLitePackageImportsNothingInRepo(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// ModeImportsOnly：只解析 import 声明，不需要文件体语义完整。
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, "godelayq/") {
				t.Errorf("%s imports %q: the common sqlite base must not depend on any in-repo package (see doc.go)", name, path)
			}
		}
	}
}
