package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// access 是一条受保护路由要求的权限，封闭枚举。
//
// 与 registry.Role 分开而不是直接把角色写进路由表：路由要表达的是「谁可以调它」，
// 而 accessSession 这条要求（任何有效会话都可以，与角色无关）不是任何一个
// 角色的名字。把它塞成「最低那个角色」会在加进第三个角色的那天悄悄变味 ——
// 会话自身的读取与销毁本来就不该随角色的增减改变。
type access int

const (
	// accessSession 只要求一个有效会话：读自己的会话、结束自己的会话。
	// 这两件事对任何已登录的人都成立，与角色无关。
	accessSession access = iota + 1
	// accessViewer 要求只读及以上，用于读取类端点。
	accessViewer
	// accessAdmin 要求管理员，用于一切创建、修改、删除、绑定、导入、
	// 覆盖、校验与设置变更（规范 §7、§28）。
	accessAdmin
)

// permits 判断持有 role 的调用方是否满足本要求。
//
// default 分支返回 false：走到那里说明加了 access 取值却没加分支，
// 而一个「没人想过它是什么意思」的权限要求只能按拒绝处理。
func (a access) permits(role registry.Role) bool {
	switch a {
	case accessSession:
		return role.Valid()
	case accessViewer:
		return role.Permits(registry.RoleViewer)
	case accessAdmin:
		return role.Permits(registry.RoleAdmin)
	default:
		return false
	}
}

// authorizer 持有每条受保护路由的权限声明，并在请求时执行它。
//
// 声明在注册路由的同一次调用里写下（见 route），因此路由表与权限表不可能
// 各自演化 —— 不存在「加了路由忘了加声明」的那半边。真正被这层结构挡住的
// 是另一种忘记：绕过 route 直接往 chi 上挂一条路由。那条路由不在声明表里，
// 于是 enforce 拒绝它（见 enforce 的默认分支）。
//
// **这层今天挡不住任何人。** 平台只有一个账号，它是管理员，因此每一次真实
// 调用都满足声明。它现在就存在，是为了第二个身份出现时不必回头补声明，
// 以及让漏声明的新端点以拒绝而不是放行的方式失败（见 registry.Role 的说明）。
type authorizer struct {
	// prefix 是这些路由挂载点的路径前缀。
	//
	// 必须与 chi 匹配到的完整模式对齐：enforce 拿到的是
	// "/api/v1/clusters/{clusterID}"，而注册时写的是 "/clusters/{clusterID}"。
	prefix string
	// declared 是「方法 + 完整路由模式」到权限要求的映射。
	declared map[string]access
}

// newAuthorizer 构造一个授权器，prefix 是受保护路由的挂载前缀。
func newAuthorizer(prefix string) *authorizer {
	return &authorizer{prefix: prefix, declared: make(map[string]access)}
}

// routeKey 是声明表的键：方法与路由模式的组合。
//
// 带上方法，是因为权限按动作分而不是按资源分：同一个地址上
// GET 与 DELETE 是两件不同性质的事。
func routeKey(method, pattern string) string {
	return method + " " + pattern
}

// route 注册一条受保护路由，并同时登记它所需的权限。
//
// 声明与注册合成一次调用，而不是在别处维护一张对照表：两处各写一遍，
// 迟早会有人只改了一处，而改漏声明的那一半是看不出来的 —— 路由照样能用。
func (a *authorizer) route(r chi.Router, method, pattern string, acc access, h http.HandlerFunc) {
	a.declared[routeKey(method, a.prefix+pattern)] = acc
	r.Method(method, pattern, h)
}

// enforce 是授权中间件：按当前会话的角色执行该路由的权限声明。
//
// **默认拒绝。** 没有声明的路由一律返回 403，而不是放行：新增端点时忘了
// 用 route 注册，症状是这个端点谁都调不通 —— 一次刺眼的失败，而不是一个
// 谁都能调的管理接口（规范 §2 Fail Secure、§5.1）。
//
// 必须装在 RequireSession 之后：它读的是会话里的角色，而角色只可能来自
// 服务端签发会话时写下的那一份（规范 §9、§34）。请求里出现的任何 role 字段
// 在这条路径上不会被读到。
//
// 403 而非 401：调用方持有有效会话，重新登录不会改变结果。前端要靠这一点
// 区分「去登录」与「你不能做这件事」。
//
// 拒绝本身不另起一条日志：RequestLogger 已经为每个请求记下
// method / path / status / code，一次 403 + 10004 在日志里就是一条可聚合的
// 拒绝记录（规范 §43）。
//
// 那条日志里**没有账号名**：RequestLogger 装在路由根部，而会话是
// RequireSession 往下游请求的 context 里放的，上游的 r 看不见它。今天这不
// 影响什么（只有一个账号），但第二个身份出现时，「谁被拒了」是审计必须
// 回答的问题。修它要动 RequestLogger 的取值方式，不属于本次改动，已在
// 报告里单列。
func (a *authorizer) enforce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := SessionFrom(r.Context())
		if !ok {
			// 会话不在上下文里说明中间件装配错了（enforce 装在了
			// RequireSession 之前）。这时既不知道调用方是谁，也就无从授权。
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeUnauthenticated)
			return
		}

		acc, declared := a.declared[routeKey(r.Method, chi.RouteContext(r.Context()).RoutePattern())]
		if !declared || !acc.permits(sess.Role) {
			response.WriteSystem(w, http.StatusForbidden, response.CodeForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}
