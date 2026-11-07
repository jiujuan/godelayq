//go:build dashboard

// 单二进制发布形态：前端产物随二进制一起编译。
// 构建顺序是先 `cd web && npm run build` 产出 web/dist，再 `go build -tags dashboard`。
package web

import (
	"embed"
	"io/fs"
)

// all: 前缀是为了连 _ 与 . 开头的文件一起收进来：public/ 目录里的这类名字
// 会被默认规则悄悄丢掉，产物缺一个文件在构建期没有任何征兆。
//
//go:embed all:dist
var distFS embed.FS

// Dist 是前端产物的根（即 web/dist 里的内容），index.html 就在这一层。
var Dist fs.FS = subOrPanic(distFS, "dist")

func subOrPanic(fsys fs.FS, dir string) fs.FS {
	out, err := fs.Sub(fsys, dir)
	if err != nil {
		// 只有 embed 指令与这里的目录名写得不一致时才会发生
		panic("web: 嵌入产物缺少 " + dir + " 目录: " + err.Error())
	}
	return out
}
