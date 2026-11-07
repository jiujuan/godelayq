//go:build !dashboard

package web

import "io/fs"

// Dist 在未带 -tags dashboard 的构建里恒为 nil。
//
// 分成两个文件而不是运行时判断，是为了让日常 `go build ./...` 与全部测试
// 完全不依赖前端产物：embed 指令在编译期就要求 web/dist 存在，否则没跑过
// npm run build 的仓库连后端都编不出来。
var Dist fs.FS
