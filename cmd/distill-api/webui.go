package main

import (
	"embed"
	"io/fs"

	"github.com/imkerbos/Distill/internal/httpapi"
)

// 前端产物打进二进制。
//
// embed 指令要求路径是写死的字面量，所以这件事只能发生在知道仓库布局的这一层，
// 不能在 internal/httpapi 里 —— 那个包不该知道前端产物放在哪。
//
// `all:` 前缀是必需的：不带它，embed 会跳过以 `.` 或 `_` 开头的文件，而
// vite 的产物里有 `.vite/` 之类的目录。跳过的东西不会报错，只会在运行时
// 表现成"少了一个文件"。
//
//go:embed all:webui
var webuiFS embed.FS

// webUI 返回可以挂到根路径上的前端产物。
//
// 产物缺失时返回 nil，而不是让进程起不来：`go build ./...` 与 `go test ./...`
// 不该要求先跑一次 npm build。nil 的含义是「这次部署不带前端」，`/` 照旧
// 回 404 —— 与加这一层之前完全一样。生产镜像里它一定存在，由 Dockerfile
// 的前端构建阶段保证。
func webUI() httpapi.WebUI {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		return nil
	}
	// 只有一个占位文件时视为没有产物：仓库里留着 .gitkeep 是为了让
	// embed 在没跑过 npm build 时也能编译。
	if entries, err := fs.ReadDir(sub, "."); err != nil || len(entries) <= 1 {
		if _, statErr := fs.Stat(sub, "index.html"); statErr != nil {
			return nil
		}
	}
	return sub
}
