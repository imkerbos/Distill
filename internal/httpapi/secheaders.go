package httpapi

import "net/http"

// SecurityHeaders 为每个响应写入一组与「这是一个只产出 JSON 的 API」
// 相称的安全响应头（规范 §37）。
//
// 刻意不照抄面向文档站点的那套预设：本服务不返回 HTML，前端由独立的
// vite 服务提供（见 CLAUDE.md「本地开发形态」）。因此每一条都写明它在
// 这个场景下实际拦住了什么，说不清效果的头一律不设 —— 一个没人能解释
// 的响应头，日后没人敢改，也没人知道它有没有在挡真实的东西。
//
// 有意**不设**的两条，理由同样记在这里，免得下次有人当成遗漏补上：
//
//   - Strict-Transport-Security：TLS 在边缘终止（Cloud Run），本进程收到
//     的是明文 HTTP，而浏览器对明文响应上的 HSTS 一律忽略；本地开发形态
//     更是纯 http。这条属于边缘配置，在这里设只会产生一个来源说不清的头。
//   - Permissions-Policy：它管的是一份文档可以使用哪些浏览器特性。本服务
//     不产出文档，说不出它在这里拦住了什么。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 本服务的响应永远不需要加载任何子资源，因此 default-src 'none'
		// 是精确描述而不是保守取值。它真正起作用的时刻是某个响应被浏览器
		// 当成文档渲染时（直接在地址栏打开某个端点、或 nosniff 之外还有
		// 别的路径）：那份 JSON 里若含调用方可控的文本，脚本与外链在这条
		// 策略下都加载不了。
		// frame-ancestors 'none' 是本条里对 XHR 响应也生效的部分，
		// 它是 §37 要的 frame protection。
		// base-uri / form-action 堵住 <base> 改写与表单提交这两条不经过
		// script-src 的注入路径。
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		// 禁止浏览器猜测响应类型。没有它，一份 Content-Type 是
		// application/json、但开头看起来像 HTML 的响应，在部分浏览器上
		// 会被当成 HTML 渲染 —— 而本 API 的响应里确实含调用方可控的文本
		// （集群 ID、校验详情）。
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// 不发送 Referer。本 API 的路径里带 clusterID 这类内部标识，
		// 一旦某个响应被当成文档打开并从中跳转，那些标识会随 Referer 外泄。
		w.Header().Set("Referrer-Policy", "no-referrer")
		// frame-ancestors 的老浏览器等价物。现代浏览器以 CSP 为准，
		// 两者结论一致（都是「谁都不许框」），不会互相矛盾。
		w.Header().Set("X-Frame-Options", "DENY")

		next.ServeHTTP(w, r)
	})
}
