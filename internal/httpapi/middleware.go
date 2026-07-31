// Package httpapi 提供 HTTP 路由、中间件与处理器。
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// RequestLogger 为每个请求输出一条完成日志。
//
// 只记录路径而不记录查询串与 Cookie：登录接口的密码可能出现在查询串里，
// 会话 token 就在 Cookie 里，两者都绝不能落盘。
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			attrs := []any{
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"code", int(rec.code),
				"duration_ms", time.Since(start).Milliseconds(),
			}
			if sess, ok := SessionFrom(r.Context()); ok {
				attrs = append(attrs, "user", sess.Username)
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

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeySession, sess)))
		})
	}
}
