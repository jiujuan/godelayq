package jobs

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本测试钉死本包的依赖红线（doc.go）：
//
//   - 生产文件（非 _test.go）不得直接 import SQLite 驱动：驱动只出现在
//     store/sqlite 底座，本包经它间接使用——与 observe/mgr 同一条红线；
//   - 生产文件不得 import internal/api 与 internal/executor：
//     接口定义在消费方（core.Store），本包只做隐式实现，反向 import 会把
//     存储层焊死在某个消费者上。
//
// 违规要在这里被点名，而不是等编译器给一条不带解释的报错。
func TestJobsPackageDoesNotImportForbidden(t *testing.T) {
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
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}

		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			switch {
			case path == "modernc.org/sqlite" || strings.HasPrefix(path, "modernc.org/sqlite/"):
				t.Errorf("%s 直接 import 了 SQLite 驱动 %q：驱动只许出现在 store/sqlite 底座", entry.Name(), path)
			case strings.HasSuffix(path, "/internal/api") || strings.HasSuffix(path, "/internal/executor"):
				t.Errorf("%s import 了消费方 %q：接口在 core 定义，本包只做隐式实现", entry.Name(), path)
			}
		}
	}
}
