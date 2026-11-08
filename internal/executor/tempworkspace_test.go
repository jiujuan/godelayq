package executor

import (
	"path/filepath"
	"testing"
)

// tempWorkspace 是测试里 t.TempDir() 的规范化替身：目录一定存在，因此
// EvalSymlinks 总能展开。CI 的 Windows 跑步机把 TEMP 指到 8.3 短名
// （C:\Users\RUNNER~1\...），而档位加载与产物清理把 workspace 折进真实长名
// 空间（profile.go 的约定：生产路径永远是 filepath.EvalSymlinks）——测试持有
// 的路径必须与生产同空间，否则 withinDirectory 的目录边界比较必然失配、
// filepath.Rel 会给出畸形爬升。
// 本机路径组件皆无短名别名时，此替身是恒等变换，既有用例行为不变。
func tempWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(real)
	}
	return dir
}
