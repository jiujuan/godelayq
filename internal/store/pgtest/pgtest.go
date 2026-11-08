// Package pgtest 是 PostgreSQL 集成测试的辅助包：从管理库连接派生一次性测试库。
//
// 门控：GODELAYQ_TEST_PG_DSN 指向一个连接串（指向哪个库都行，只要连接有建库权限，
// 通常是维护库 postgres）。未配置时 Database 打印指引并 t.Skip——没有 PG 的环境
// （无 Docker 的开发机、CI 的快速档）测试集照常全绿，配置了才真正跑。
//
// 隔离：每个调用创建独立的 godelayq_test_<用途>_<随机> 库并在测试结束 DROP WITH (FORCE)，
// go test 并行跑包时同名表互不撞车。口令只经环境变量传递，任何文件不得落明文。
package pgtest

import (
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jiujuan/godelayq/internal/store/engine"
	"github.com/stretchr/testify/require"
)

// adminEnvVar 是管理库连接串的环境变量名。指向的库只用来 CREATE/DROP 测试库。
const adminEnvVar = "GODELAYQ_TEST_PG_DSN"

// AdminDSN 返回管理库连接串；第二个返回值为 false 表示未配置（测试应跳过）。
func AdminDSN() (string, bool) {
	dsn := strings.TrimSpace(os.Getenv(adminEnvVar))
	return dsn, dsn != ""
}

// Database 为一次测试创建一个一次性 PostgreSQL 库并返回它的连接串。
// purpose 只允许字母数字下划线（进库名），调用方用它区分用途（jobs/mgr/observe/server）。
func Database(t *testing.T, purpose string) string {
	t.Helper()

	adminDSN, ok := AdminDSN()
	if !ok {
		t.Skipf("set %s (e.g. postgres://user:pass@localhost:5432/postgres?sslmode=disable) to run the PostgreSQL suite", adminEnvVar)
	}
	for _, r := range purpose {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			t.Fatalf("pgtest purpose %q must be [a-z0-9_]", purpose)
		}
	}

	admin, err := engine.Open(engine.Options{Dialect: engine.DialectPostgres, DSN: adminDSN}, nil)
	require.NoError(t, err)

	// 库名由本包代码生成（固定前缀 + 受限字符集 + 随机数），直接拼接没有注入面。
	name := fmt.Sprintf("godelayq_test_%s_%06d", purpose, rand.Intn(1000000))
	// CREATE/DROP DATABASE 不能在事务里跑，直连执行。
	_, err = admin.SQL().Exec(`CREATE DATABASE ` + name)
	require.NoError(t, err)
	t.Cleanup(func() {
		// WITH (FORCE) 先断开残留连接再删（PG 13+），重跑不积垃圾。
		_, _ = admin.SQL().Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
		_ = admin.Close()
	})

	return withDatabaseName(adminDSN, name)
}

// withDatabaseName 把连接串指向的库换成 name。支持 URL 形态与 keyword/value 形态
//（pgx 两者都收）；两种都认不出时返回错误——宁可让测试失败也不要把行写进维护库。
func withDatabaseName(dsn, name string) string {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		u.Path = "/" + name
		return u.String()
	}
	if regexp.MustCompile(`(?i)\bdbname\s*=`).MatchString(dsn) {
		return regexp.MustCompile(`(?i)\bdbname\s*=\s*\S+`).ReplaceAllString(dsn, "dbname="+name)
	}
	return dsn + " dbname=" + name
}
