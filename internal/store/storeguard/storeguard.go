// Package storeguard 是存储层红线守卫的共享实现。与 pgtest 同类：测试辅助包，
// 只应被 _test.go 引用，不进入任何生产 import 链。
//
// 规则作用于"生产文件"（目录下非 _test.go 的 .go 文件），走 go/ast 而不是字符串
// 匹配——与各包 package_guard_test 的既有取向一致：违规要在测试期被点名带文件号，
// 而不是等某一方言在线上炸掉。各业务包的 package_guard_test.go 调用本包并给出
// 自己的白名单。
//
// 已知局限（有意接受，写在规则注释里）：经变量中转的拼接拦不住，门禁抓的是
// "直接把 Sprintf 塞进构建器/查询出口"这一类高频捷径；完美防注入靠的是
// "参数化查询"这条已经在全仓成立的事实。
package storeguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rawSQLSelectors 是"原生查询出口"的选择器名：SQL() 逃生口本身，以及
// *sql.DB 经嵌入提升出来的 Query/QueryRow 家族——它们都会绕过 bun 构建器
// 直接执行手写 SQL。
var rawSQLSelectors = map[string]bool{
	"SQL":              true,
	"Query":            true,
	"QueryRow":         true,
	"QueryContext":     true,
	"QueryRowContext":  true,
}

// dialectIdents 是方言分支标识符：查询层按方言分叉只允许出现在各包自己的
// 白名单文件里（当前全仓唯一合法点是 mgr 的 nameConflictClause）。
var dialectIdents = map[string]bool{
	"DialectPostgres": true,
	"DialectSQLite":   true,
}

// builderMethods 是 bun 构建器上"接 SQL 文本"的方法：它们的参数必须是
// 常量字面量（带 ?/$N 占位符 + 参数），不允许 fmt.Sprintf 动态拼出来。
var builderMethods = map[string]bool{
	"Where":       true,
	"WhereExpr":   true,
	"On":          true,
	"Set":         true,
	"Order":       true,
	"OrderExpr":   true,
	"ColumnExpr":  true,
	"GroupExpr":   true,
	"Having":      true,
	"Exec":        true,
	"ExecContext": true,
}

// GuardNoRawSQL 断言目录下生产文件没有原生查询出口，allowFiles 里的文件基名豁免
// （合法出口必须逐个登记，注释说明为什么非它不可）。
func GuardNoRawSQL(t *testing.T, dir string, allowFiles map[string]bool) {
	t.Helper()
	walk(t, dir, "raw query outlet (.SQL()/Query/QueryRow…)", allowFiles,
		func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			return ok && rawSQLSelectors[sel.Sel.Name]
		})
}

// GuardNoDialectBranch 断言目录下生产文件不引用方言标识符，allowFiles 豁免。
func GuardNoDialectBranch(t *testing.T, dir string, allowFiles map[string]bool) {
	t.Helper()
	walk(t, dir, "dialect branch (DialectPostgres/DialectSQLite)", allowFiles,
		func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			return ok && dialectIdents[id.Name]
		})
}

// GuardNoDynamicSQL 断言构建器方法的直接参数没有 fmt.Sprintf 调用。
// 只查直接实参：经变量中转的拼接不在此列（见包注释的局限声明）。
func GuardNoDynamicSQL(t *testing.T, dir string) {
	t.Helper()
	walk(t, dir, "dynamic SQL (fmt.Sprintf into a builder method)", nil,
		func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return false
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !builderMethods[sel.Sel.Name] {
				return false
			}
			for _, arg := range call.Args {
				if isFmtSprintf(arg) {
					return true
				}
			}
			return false
		})
}

// isFmtSprintf 报告表达式是不是 fmt.Sprintf(...) 调用。
func isFmtSprintf(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt" && sel.Sel.Name == "Sprintf"
}

// walk 解析 dir 下的生产文件，predicate 命中的节点按文件：行号报错。
// allowFiles 以文件基名为键；空表表示零豁免。
func walk(t *testing.T, dir, what string, allowFiles map[string]bool, predicate func(ast.Node) bool) {
	t.Helper()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("storeguard: read %s: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("storeguard: parse %s: %v", name, err)
		}
		allowed := allowFiles != nil && allowFiles[name]
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				return true
			}
			if predicate(n) {
				pos := fset.Position(n.Pos())
				if !allowed {
					t.Errorf("%s:%d: %s is forbidden in production code (see the package guard's comment)",
						pos.Filename, pos.Line, what)
				}
			}
			return true
		})
	}
}
