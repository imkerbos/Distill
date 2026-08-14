package httpapi

import (
	"context"
	"log/slog"
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

// roleResolver 按用户名现读角色。*auth.Verifier 满足它。
//
// 收窄成一个方法而不是直接依赖 *auth.Verifier：授权层要问的只有
// "这个账号此刻是什么角色"，而校验器还带着登录与用户名校验。
//
// 每次判定都调一次，结果不进会话：会话在签发时抄一份角色的话，一个刚被
// 降权或停用的账号会继续拿着原权限直到会话过期，默认 8 小时
// （design doc 2026-08-14 §4）。
type roleResolver interface {
	// RoleOf 返回 username 此刻生效的角色。账号不存在、已停用、已软删除，
	// 或角色不在封闭枚举内时，第二个返回值为 false。
	RoleOf(ctx context.Context, username string) (registry.Role, bool, error)
}

// withRole 把本次请求现读出来的角色放进 context。
//
// 供 handler 回答"我是什么角色"（GET /sessions/current，design doc §6）。
// 只有 enforce 调它，值只可能来自 roleResolver —— 请求体、请求头、Cookie
// 与查询串里出现的任何 role 字段都到不了这里（规范 §9、§34）。
func withRole(ctx context.Context, role registry.Role) context.Context {
	return context.WithValue(ctx, ctxKeyRole, role)
}

// roleFrom 取出本次请求已判定的角色，缺失时第二个返回值为 false。
func roleFrom(ctx context.Context) (registry.Role, bool) {
	role, ok := ctx.Value(ctxKeyRole).(registry.Role)
	return role, ok
}

// authorizer 持有每条受保护路由的权限声明，并在请求时执行它。
//
// 声明在注册路由的同一次调用里写下（见 route），因此路由表与权限表不可能
// 各自演化 —— 不存在「加了路由忘了加声明」的那半边。真正被这层结构挡住的
// 是另一种忘记：绕过 route 直接往 chi 上挂一条路由。那条路由不在声明表里，
// 于是 enforce 拒绝它（见 enforce 的默认分支）。
//
// 账号落库之后这层开始挡人：角色不再是配置文件里那个恒为管理员的取值，
// 而是每次请求从账号记录现读出来的（见 roleResolver）。
type authorizer struct {
	// prefix 是这些路由挂载点的路径前缀。
	//
	// 必须与 chi 匹配到的完整模式对齐：enforce 拿到的是
	// "/api/v1/clusters/{clusterID}"，而注册时写的是 "/clusters/{clusterID}"。
	prefix string
	// declared 是「方法 + 完整路由模式」到权限要求的映射。
	declared map[string]access
	// roles 现读角色。
	roles roleResolver
	// logger 只用于记角色读不出来时的原因：那是一次依赖失败，调用方看到的
	// 是一句固定文案，真实原因带 request_id 进日志（规范 §22）。
	logger *slog.Logger
}

// newAuthorizer 构造一个授权器，prefix 是受保护路由的挂载前缀。
func newAuthorizer(prefix string, roles roleResolver, logger *slog.Logger) *authorizer {
	return &authorizer{
		prefix:   prefix,
		declared: make(map[string]access),
		roles:    roles,
		logger:   logger,
	}
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
// 必须装在 RequireSession 之后：它要先知道调用方是谁，才谈得上现读那个
// 账号此刻的角色。角色只来自账号记录（规范 §9、§34）——请求体、请求头、
// Cookie 与查询串里出现的任何 role 字段在这条路径上不会被读到。
//
// 403 而非 401：调用方持有有效会话，重新登录不会改变结果。前端要靠这一点
// 区分「去登录」与「你不能做这件事」。
//
// 拒绝本身不另起一条日志：RequestLogger 已经为每个请求记下
// method / path / status / code **与账号名**，一次 403 + 10004 在日志里
// 就是一条可聚合、且答得出"谁"的拒绝记录（design doc 2026-08-14 §7，
// 规范 §43）。账号名的回填见 requestActor。
func (a *authorizer) enforce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := SessionFrom(r.Context())
		if !ok {
			// 会话不在上下文里说明中间件装配错了（enforce 装在了
			// RequireSession 之前）。这时既不知道调用方是谁，也就无从授权。
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeUnauthenticated)
			return
		}

		role, resolved, err := a.roles.RoleOf(r.Context(), sess.Username)
		if err != nil {
			// 读不出角色不是"没有限制"：一次数据库抖动不该变成一次放行
			// （规范 §49）。也不是 403 —— 我们并没有判定这个账号权限不足，
			// 而是根本没能判定，把依赖故障说成权限不足会让操作者去找管理员
			// 要权限，而问题在别处。
			a.logger.Error("cannot resolve the role for this session",
				"err", err, "request_id", RequestIDFrom(r.Context()), "user", sess.Username)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}
		if !resolved {
			// 账号被停用、被软删除，或它的角色不在封闭枚举里：这张会话
			// 此刻不对应任何一个能做事的身份，因此立即失效，不必等它过期，
			// 也不必有谁记得去逐个撤销（design doc 2026-08-14 §4）。
			//
			// 401 + 会话过期码而不是 403：这不是"你不能做这件事"，是
			// "你这张会话不再代表任何人"，前端的处置是回登录页。
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeSessionExpired)
			return
		}

		acc, declared := a.declared[routeKey(r.Method, chi.RouteContext(r.Context()).RoutePattern())]
		if !declared || !acc.permits(role) {
			response.WriteSystem(w, http.StatusForbidden, response.CodeForbidden)
			return
		}

		// 角色顺着 context 往下游走：handler 要回答"我是什么角色"时
		// （GET /sessions/current）就不必再读一次账号表，也不可能读出与
		// 这次判定不一致的另一个值。
		next.ServeHTTP(w, r.WithContext(withRole(r.Context(), role)))
	})
}
