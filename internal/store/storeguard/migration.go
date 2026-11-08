package storeguard

// migration.go 承载迁移纪律守卫（G7/Q11）。与同包三条 AST 红线互补：
// 那三条扫生产文件的代码形状，这三条查**迁移清单本身**——输入是调用方包里的
// []engine.Migration，规则是清单级检查，不需要解析 AST。
//
// 防的事故各有一段现场：
//   - 版本递增：版本号写重/乱序会让 Migrate 的"只跑比库里已应用版本更高的那些"
//     判定失真——重复版本意味着两份互斥的 DDL 抢同一个版本位，乱序意味着跳版执行；
//   - 方言可移植：Q04（CI 首跑）暴露过 SQLite 专属写法混进 PG DDL——AUTOINCREMENT
//     在 PG 是语法错误，带自增主键的 PG DDL 必须写 IDENTITY。规则是它的泛化，
//     把"下一次 Q04"拦在测试期；
//   - v1 冻结：既有部署迁移判定的起点是 v1 语句原文，"顺手整理"（换行、空白、
//     同义改写）都可能改变后续版本的适用前提——golden 逐字节冻结，变更必须
//     diff 评审后显式 -update。
//
// 规则形态：Check* 返回问题清单而不是直接 t.Error——"人为违规必红"的自证用
// 构造好的坏输入直接断言问题非空（永久留作回归），不必临时改生产清单再还原。
// Require* 是各包 package_guard_test 的接线包装，把问题点名到测试输出。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiujuan/godelayq/internal/store/engine"
)

// CheckMigrationVersions 校验版本号从 1 开始、严格递增、无重复。
func CheckMigrationVersions(ms []engine.Migration) []string {
	var problems []string
	for i, m := range ms {
		if m.Version <= 0 {
			problems = append(problems,
				fmt.Sprintf("migration #%d has non-positive version %d", i, m.Version))
			continue
		}
		if i > 0 && m.Version <= ms[i-1].Version {
			problems = append(problems,
				fmt.Sprintf("migration version %d is not strictly greater than the previous %d (duplicate or out of order)",
					m.Version, ms[i-1].Version))
		}
	}
	return problems
}

// CheckMigrationPortable 校验 UpPostgres 语句的方言可移植性：出现 AUTOINCREMENT
// 的语句必须同句含 IDENTITY。大小写不敏感；Up（两方言共用的通用语句集）不查——
// 里面写 SQLite 专属语法会在 PG 迁移时当场炸（响亮故障），不是静默事故；
// UpPostgres 才是"替 PG 执行、错了要等迁移跑到才炸"的那一份。
func CheckMigrationPortable(ms []engine.Migration) []string {
	var problems []string
	for _, m := range ms {
		for _, stmt := range m.UpPostgres {
			lower := strings.ToLower(stmt)
			if strings.Contains(lower, "autoincrement") && !strings.Contains(lower, "identity") {
				problems = append(problems,
					fmt.Sprintf("migration version %d: UpPostgres statement uses AUTOINCREMENT without IDENTITY (SQLite-only syntax; PostgreSQL needs IDENTITY, see Q04)", m.Version))
			}
		}
	}
	return problems
}

// CheckMigrationV1Frozen 把 version 1 的 Up（及非空的 UpPostgres）语句文本与
// golden 逐字节比对。goldenPath 相对调用方测试目录（testdata/migration_v1.golden）；
// update 为 true 时重写 golden（go test ./internal/store/<pkg>/ -run TestPackageGuard -update）。
// 引擎的 Migration 没有 Down（迁移只进不退，回滚靠备份），冻结面就是 Up 两份。
func CheckMigrationV1Frozen(ms []engine.Migration, goldenPath string, update bool) ([]string, error) {
	if len(ms) == 0 || ms[0].Version != 1 {
		return []string{"migration list must start at version 1 for the frozen-golden check"}, nil
	}
	got := migrationV1Text(ms[0])
	if update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			return nil, fmt.Errorf("create golden dir: %w", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o640); err != nil {
			return nil, fmt.Errorf("write migration v1 golden: %w", err)
		}
		return nil, nil
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		return nil, fmt.Errorf("migration v1 golden missing: run go test ./internal/store/<pkg>/ -run TestPackageGuard -update to generate")
	}
	if string(want) != got {
		return []string{"version-1 migration text drifted from the frozen golden; existing deployments judge their schema by this text — review the diff and regenerate only if the change is truly intended"}, nil
	}
	return nil, nil
}

// migrationV1Text 把 v1 迁移折成确定性文本：节标题 + 每语句一行。
func migrationV1Text(m engine.Migration) string {
	var b strings.Builder
	b.WriteString("version 1 up:\n")
	for _, stmt := range m.Up {
		b.WriteString(stmt)
		b.WriteString("\n")
	}
	if len(m.UpPostgres) > 0 {
		b.WriteString("version 1 up_postgres:\n")
		for _, stmt := range m.UpPostgres {
			b.WriteString(stmt)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// RequireMigrationVersions 接线包装：版本纪律问题逐条点名。
func RequireMigrationVersions(t *testing.T, ms []engine.Migration) {
	t.Helper()
	for _, p := range CheckMigrationVersions(ms) {
		t.Error(p)
	}
}

// RequireMigrationPortable 接线包装：方言可移植问题逐条点名。
func RequireMigrationPortable(t *testing.T, ms []engine.Migration) {
	t.Helper()
	for _, p := range CheckMigrationPortable(ms) {
		t.Error(p)
	}
}

// RequireMigrationV1Frozen 接线包装：v1 冻结比对，读取失败按致命处理
// （golden 缺失说明生成步骤被跳过，修文件解决不了）。
func RequireMigrationV1Frozen(t *testing.T, ms []engine.Migration, goldenPath string, update bool) {
	t.Helper()
	problems, err := CheckMigrationV1Frozen(ms, goldenPath, update)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
