package jobs

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiujuan/godelayq/internal/store/storeguard"
)

// 本测试钉死本包的依赖红线（doc.go）：
//
//   - 生产文件（非 _test.go）不得直接 import SQLite 驱动：驱动只出现在
//     store/engine 底座，本包经它间接使用——与 observe/mgr 同一条红线；
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
				t.Errorf("%s 直接 import 了 SQLite 驱动 %q：驱动只许出现在 store/engine 底座", entry.Name(), path)
			case strings.HasSuffix(path, "/internal/api") || strings.HasSuffix(path, "/internal/executor"):
				t.Errorf("%s import 了消费方 %q：接口在 core 定义，本包只做隐式实现", entry.Name(), path)
			}
		}
	}
}

// TestJobsPackageStoreRedlines 钉住 bun 化后的三条存储红线（db-quality-gates-design.md §4）：
// 生产代码没有原生查询出口（SQL()/Query 家族）、没有方言分支标识符、构建器参数
// 没有 fmt.Sprintf 动态拼接。三条全部零豁免：任务库的每一类操作都已有构建器写法。
// 经变量中转的拼接拦不住（storeguard 包注释的局限声明），门禁抓的是高频捷径。
func TestJobsPackageStoreRedlines(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}
	storeguard.GuardNoRawSQL(t, dir, nil)
	storeguard.GuardNoDialectBranch(t, dir, nil)
	storeguard.GuardNoDynamicSQL(t, dir)
}
