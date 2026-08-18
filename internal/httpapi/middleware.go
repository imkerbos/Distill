// Package httpapi 提供 HTTP 路由、中间件与处理器。
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/response"
)

// SessionCookieName 是会话 Cookie 的名字。
const SessionCookieName = "distill_session"

// ctxKey 是本包私有的 context key 类型，避免与其他包冲突。
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeySession
	ctxKeyActor
	// ctxKeyRole 携带本次请求现读出来的角色。写入方是 authz.go 的授权
	// 中间件，键定义在这里只是为了让全部 context 键留在同一处 ——
	// 分散之后，"这个包一共往 context 里放了几样东西"就没有一处答得上来。
	ctxKeyRole
	// ctxKeyAgentCluster 携带 agent 认证判定出的集群归属。写入方只有
	// RequireAgent，读取方只有 AgentClusterFrom（design doc 2026-08-18 §2）。
	//
	// 与 ctxKeySession 分成两个键：两条认证链的产物不得互相冒充 —— 共用
	// 一个键，一次装配错误就会让人的会话在摄入路径上被读成一个 agent 身份。
	ctxKeyAgentCluster
)

// RequestID 为每个请求生成标识，并回写到响应头。
//
// 用户报障时只需提供这个 ID 就能定位到具体一次请求的完整日志，
// 无需描述"大概几点点了哪个按钮"。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			// 生成失败不该让请求失败；退化成空 ID，日志里仍有其余字段。
			next.ServeHTTP(w, r)
			return
		}
		id := hex.EncodeToString(raw)

		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// RequestIDFrom 从上下文取出请求标识，缺失时返回空串。
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// statusRecorder 记录实际写出的状态码与业务码，供日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	code   response.Code
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// RecordCode 实现 response.CodeRecorder：业务失败一律 200，
// 只有把 code 记进日志，运维才能在状态码之外看到另一半失败。
func (s *statusRecorder) RecordCode(code response.Code) {
	s.code = code
}

// requestActor 是本次请求的账号信箱。
//
// 它存在的理由是中间件的装配次序：RequestLogger 在路由根部，而会话要到
// RequireSession 才认得出来，且是往**下游**派生的 context 里放的 ——
// 上游手里的 r 永远看不到它。原先那句 `if sess, ok := SessionFrom(...)`
// 因此是一条从不成立的死分支，日志里的账号名字段从未落过盘
// （design doc 2026-08-14 §7）。
//
// 用一个指针信箱而不是把日志中间件挪到认证之后：挪下去之后，401 与
// 未匹配到路由的请求就不再产生完成日志，而"谁在被拒"正是审计要问的
// 那一半（规范 §43）。
//
// 只放账号名。会话 ID 是凭据，进日志等于把它复制进一份长期留存、
// 且比内存更容易被导出的地方（规范 §21）。
type requestActor struct {
	username string
}

// recordActor 把已认证的账号名写进本次请求的信箱；没有信箱时什么都不做。
//
// 不加锁：信箱在一次请求的处理 goroutine 内创建、写入、读取，写入发生在
// next.ServeHTTP 之内，读取发生在它返回之后，两者之间有 happens-before。
func recordActor(ctx context.Context, username string) {
	if a, ok := ctx.Value(ctxKeyActor).(*requestActor); ok {
		a.username = username
	}
}

// RequestLogger 为每个请求输出一条完成日志。
//
// 只记录路径而不记录查询串与 Cookie：登录接口的密码可能出现在查询串里，
// 会话 token 就在 Cookie 里，两者都绝不能落盘。
//
// 已认证的请求还带上账号名：没有它，第 5 节那条"最后一个管理员"的保护
// 即使触发，事后也说不清是谁触发的；授权拒绝（403）同样必须对得上账号
// （design doc 2026-08-14 §7）。账号名由 RequireSession 通过 requestActor
// 回填 —— 见该类型的注释。
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			actor := &requestActor{}
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), ctxKeyActor, actor)))

			attrs := []any{
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"code", int(rec.code),
				"duration_ms", time.Since(start).Milliseconds(),
			}
			// 未认证的请求不写这个字段，而不是写一个空串："user":"" 会在
			// 日志聚合里被当成一个真实存在的账号名。
			if actor.username != "" {
				attrs = append(attrs, "user", actor.username)
			}
			logger.Info("request completed", attrs...)
		})
	}
}

// Recoverer 拦截 handler 里逃逸出来的 panic。
//
// 必须装在 RequestID 与 RequestLogger 之后：panic 若一路冲出中间件栈，
// 连接会被直接掐断，既没有响应包络，也不会产生那条完成日志——用户拿着
// X-Request-Id 来报障时，日志里什么都查不到；panic 的堆栈还会以非 JSON
// 的纯文本打到 stderr，破坏"日志全是 JSON"这条对采集侧的保证。
//
// panic 值本身只进日志，绝不回给调用方：它常常带着内部地址与文件路径。
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					// 标准库约定：这个哨兵表示"静默中止"，不该被当成 panic 记录。
					panic(rec)
				}
				logger.Error("panic recovered",
					"request_id", RequestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()))
				response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// SessionFrom 从上下文取出已认证的会话。
func SessionFrom(ctx context.Context) (auth.Session, bool) {
	sess, ok := ctx.Value(ctxKeySession).(auth.Session)
	return sess, ok
}

// RequireSession 拦截未携带有效会话的请求。
//
// 返回 401 而非 200 + code：未登录是协议层的失败，浏览器与前端拦截器
// 都按状态码判断，吞进响应体会让每个请求都要解析后才知道该跳登录页。
func RequireSession(sessions *auth.SessionStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(SessionCookieName)
			if err != nil || cookie.Value == "" {
				response.WriteSystem(w, http.StatusUnauthorized, response.CodeUnauthenticated)
				return
			}

			sess, ok := sessions.Get(cookie.Value)
			if !ok {
				// 有 Cookie 但会话无效，绝大多数情况是过期。
				// 用独立的码让界面能提示"会话已过期"而不是"请先登录"。
				response.WriteSystem(w, http.StatusUnauthorized, response.CodeSessionExpired)
				return
			}

			// 身份在这里第一次成立，因此回填信箱也只能在这里：
			// 请求日志、授权拒绝与审计要回答的"谁"，全都指向这一个值
			// （design doc 2026-08-14 §7）。
			recordActor(r.Context(), sess.Username)

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeySession, sess)))
		})
	}
}
