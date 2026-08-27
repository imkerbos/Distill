package httpapi

import "net/http"

// apiOnlyCSP 是这个服务只产出 JSON 时的策略。
//
// `default-src 'none'` 是精确描述而不是保守取值：那种部署形态下的响应
// 永远不需要加载任何子资源。它真正起作用的时刻是某个响应被浏览器当成文档
// 渲染时 —— 那份 JSON 里若含调用方可控的文本，脚本与外链在这条策略下都
// 加载不了。
const apiOnlyCSP = "default-src 'none'; frame-ancestors 'none'; " +
	"base-uri 'none'; form-action 'none'"

// webUICSP 是同一个进程还要服务前端时的策略。
//
// 逐条放行前端真正需要的东西，**不退回 `default-src 'self'`**：后者会连带
// 放开 object-src、frame-src 这些前端根本不用的取值，而它们正是注入之后最
// 好用的几条路径。`default-src 'none'` 仍然是兜底，凡是没在下面列出来的
// 一律加载不了。
//
// style-src 带 'unsafe-inline'：这一屏的组件大量使用 React 的 style 属性
// （判定语义色、卡片间距），而 Tailwind 的运行时也会插入 <style>。没有它
// 整个界面会以无样式的形态渲染出来 —— 那比报错更糟，因为它看起来"打开了"。
//
// connect-src 'self'：前端与 API 同源，XHR 只打自己。
// img-src 带 data:：图标与内联的小图是 data URI。
const webUICSP = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// SecurityHeaders 为每个响应写入一组安全响应头（规范 §37）。
//
// servesWebUI 决定用哪一份 CSP。**两份而不是一份**：只产出 JSON 的部署配得上
// `default-src 'none'`，而把它照搬到带前端的部署上，会把自己的脚本、样式和
// 图标全部挡掉 —— 界面渲染成一片空白，控制台里全是 CSP 违规。这不是假设：
// 第一次真部署就是这个样子，而本机开发形态（前端跑在独立的 vite server 上）
// 永远碰不到它。
//
// 每一条都写明它在这个场景下实际拦住了什么，说不清效果的头一律不设 ——
// 一个没人能解释的响应头，日后没人敢改，也没人知道它有没有在挡真实的东西。
//
// 有意**不设**的两条，理由同样记在这里，免得下次有人当成遗漏补上：
//
//   - Strict-Transport-Security：TLS 可能在边缘终止，那时本进程收到的是明文
//     HTTP，而浏览器对明文响应上的 HSTS 一律忽略；本地开发形态更是纯 http。
//     这条属于边缘配置，在这里设只会产生一个来源说不清的头。
//   - Permissions-Policy：它管的是一份文档可以使用哪些浏览器特性。这个服务
//     产出的文档只有前端自己那一份，说不出它在这里额外拦住了什么。
func SecurityHeaders(servesWebUI bool) func(http.Handler) http.Handler {
	csp := apiOnlyCSP
	if servesWebUI {
		csp = webUICSP
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// frame-ancestors 'none' 是本条里对 XHR 响应也生效的部分，
			// 它是 §37 要的 frame protection。
			// base-uri / form-action 堵住 <base> 改写与表单提交这两条不经过
			// script-src 的注入路径。
			w.Header().Set("Content-Security-Policy", csp)
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
}
