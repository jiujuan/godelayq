package mgr

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
//   - 生产文件（非 _test.go）不得直接 import 数据库驱动（modernc.org/sqlite 与
//     pgx/v5）：驱动只出现在 store/engine 底座，本包经它间接使用——与 jobs/observe
//     同一条红线（此前只有注释没有守卫，本文件补齐）；
//   - 生产文件不得 import internal/api 与 internal/executor：接口定义在消费方
//     （core.UserStore/GroupStore/ExecutorProfileStore），本包只做隐式实现。
//
// 违规要在这里被点名，而不是等编译器给一条不带解释的报错。
func TestMgrPackageDoesNotImportForbidden(t *testing.T) {
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
			case strings.HasPrefix(path, "github.com/jackc/pgx"):
				t.Errorf("%s 直接 import 了 PostgreSQL 驱动 %q：驱动只许出现在 store/engine 底座", entry.Name(), path)
			case strings.HasSuffix(path, "/internal/api") || strings.HasSuffix(path, "/internal/executor"):
				t.Errorf("%s import 了消费方 %q：接口在 core 定义，本包只做隐式实现", entry.Name(), path)
			}
		}
	}
}

// TestMgrPackageStoreRedlines 钉住 bun 化后的三条存储红线（db-quality-gates-design.md §4）。
// 方言分支的唯一合法点是 models.go 的 nameConflictClause：大小写不敏感唯一在两个方言
// 上的表达不同（SQLite COLLATE NOCASE / PG lower(name) 唯一索引），全仓只允许这一处
// 按方言给 upsert 冲突目标。原生查询出口与动态拼接零豁免。
func TestMgrPackageStoreRedlines(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}
	storeguard.GuardNoRawSQL(t, dir, nil)
	storeguard.GuardNoDialectBranch(t, dir, map[string]bool{"models.go": true})
	storeguard.GuardNoDynamicSQL(t, dir)
}

// 迁移纪律守卫（G7/Q11）：版本严格递增无重复、UpPostgres 方言可移植
// （AUTOINCREMENT 必须配 IDENTITY）、version 1 语句文本逐字节冻结。
// v1 golden 的重写通道与 SQL 快照同款：
//
//	go test ./internal/store/mgr/ -run TestPackageGuard -update
//
// 冻结的是既有部署迁移判定的起点，变更必须 diff 评审后显式更新。
func TestMgrMigrationDiscipline(t *testing.T) {
	storeguard.RequireMigrationVersions(t, migrations)
	storeguard.RequireMigrationPortable(t, migrations)
	storeguard.RequireMigrationV1Frozen(t, migrations,
		filepath.Join("testdata", "migration_v1.golden"), *updateSnapshot)
}
