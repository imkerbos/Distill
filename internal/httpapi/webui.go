package httpapi

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/imkerbos/Distill/internal/response"
)

// 前端产物的服务层。
//
// **本机开发时前端不经过这里**：它跑在独立的 vite dev server 上，`/api` 走
// proxy 打后端（CLAUDE.md 的本地开发形态）。这一层是给生产镜像用的 ——
// 在这之前生产镜像里根本没有前端，`/` 返回一句「请求的资源不存在」，而
// 开发形态永远碰不到那条路径，所以它一直没被发现。
//
// 与前端同源、同端口、同一份 TLS，是刻意的：会话 cookie 是 HttpOnly +
// SameSite，分开部署会把开发时特意绕开的跨源问题原样搬进生产 —— 那时要么
// 配 CORS + SameSite=None，要么再给前端配一次证书。

// WebUI 是前端构建产物（web/dist）。
//
// 由调用方注入而不是在本包 embed：`internal/httpapi` 不该知道前端产物放在
// 仓库的哪个位置，而 embed 指令要求路径写死在源码里。nil 表示这次部署不带
// 前端 —— 那时 `/` 照旧回 404，与从前一样。
type WebUI fs.FS

// mountWebUI 把前端挂在根路径上，作为 NotFound 的兜底。
//
// **只兜 GET/HEAD，且只在路径不以 API 前缀开头时**：一个打错的 API 路径必须
// 拿到 JSON 的 404，而不是一份 index.html —— 前者说得出「这个接口不存在」，
// 后者会让调用方以为自己拿到了数据，直到 JSON 解析失败。
//
// 找不到文件时回 index.html 而不是 404：前端是单页应用，`/clusters` 这类
// 路径在产物里没有对应文件，由浏览器侧的路由接管。这条 fallback 不能对
// 静态资源生效 —— 一个拼错的 .js 路径回 index.html，浏览器会把 HTML 当
// JavaScript 解析，报出的错与真正的成因毫无关系。
func mountWebUI(ui WebUI) func(http.ResponseWriter, *http.Request) {
	files := http.FileServer(http.FS(ui))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			response.WriteSystem(w, http.StatusNotFound, response.CodeNotFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, apiPrefix) {
			response.WriteSystem(w, http.StatusNotFound, response.CodeNotFound)
			return
		}

		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" || clean == "." {
			clean = "index.html"
		}
		if _, err := fs.Stat(ui, clean); err != nil {
			// 带扩展名的路径找不到就是找不到：那是一个静态资源请求，
			// 回 index.html 只会把成因藏起来。
			if path.Ext(clean) != "" {
				response.WriteSystem(w, http.StatusNotFound, response.CodeNotFound)
				return
			}
			// 其余交给单页应用自己的路由。
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	}
}
