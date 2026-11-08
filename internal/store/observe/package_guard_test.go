package observe

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiujuan/godelayq/internal/store/storeguard"
)

// 本测试钉死本包的两条依赖红线（doc.go）：
//
//   - 生产文件（非 _test.go）不得 import godelayq/api 与 godelayq/executor：
//     接口定义在消费方，本包只做隐式实现；反向 import 会把存储层焊死在某个
//     消费者上，也与"满足关系由装配方编译期把关"的口径冲突。
//   - 与 store/engine 的 TestSQLitePackageImportsNothingInRepo 同一取向：
//     违规要在这里被点名，而不是等编译器给一条不带解释的报错。
//
// 测试文件例外：artifacts_test.go import executor 构造真实的 ArtifactStore
// 做索引集成验证，测试期依赖不进生产依赖图，所以守卫跳过 _test.go。
func TestObservePackageDoesNotImportConsumers(t *testing.T) {
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
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "github.com/jiujuan/godelayq/internal/api" || path == "github.com/jiujuan/godelayq/internal/executor" {
				t.Errorf("%s imports %q: the storage package must not import its consumers (see doc.go)", name, path)
			}
		}
	}
}

// TestObservePackageStoreRedlines 钉住 bun 化后的三条存储红线（db-quality-gates-design.md §4）。
// 豁免仅 db.go：Stats 是三库共用的可移植原生语句（COALESCE(MAX)/COUNT），不走构建器；
// 其余事件/审计/工件读写全部经构建器。方言分支零豁免。
func TestObservePackageStoreRedlines(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}
	storeguard.GuardNoRawSQL(t, dir, map[string]bool{"db.go": true})
	storeguard.GuardNoDialectBranch(t, dir, nil)
	storeguard.GuardNoDynamicSQL(t, dir)
}
