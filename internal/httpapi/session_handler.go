package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/imkerbos/Distill/internal/response"
)

// loginRequest 是登录请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleCreateSession 处理登录。
//
// 凭证错误返回 HTTP 200 + 10001：这是业务级失败，不该计入服务错误率。
// 请求体无法解析则返回 400 —— 那是协议层的问题。
func handleCreateSession(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
			return
		}

		role, ok := d.Verifier.Verify(req.Username, req.Password)
		if !ok {
			// 用户不存在与密码错误返回完全相同的响应，避免账号枚举。
			response.WriteBusiness(w, response.CodeInvalidCredentials)
			return
		}

		// 角色来自账号记录，由校验器一并给出 —— 不读请求体里的任何字段
		// （规范 §9、§34）。Create 会拒绝未登记的角色，因此签不出一个
		// 「已认证但没有角色」的会话。
		sess, err := d.Sessions.Create(req.Username, role)
		if err != nil {
			d.Logger.Error("create session failed",
				"request_id", RequestIDFrom(r.Context()), "error", err)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}

		// Secure 故意不设：本地开发形态是前端 vite 走明文 http 代理到本服务
		// （见 CLAUDE.md「本地开发形态」），Secure Cookie 在 http://localhost
		// 上不会被浏览器接受，设了反而直接破坏登录。HttpOnly+SameSite 已经
		// 覆盖了这里真正要防的两类风险（脚本读取、跨站请求）。
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure omitted intentionally, see comment above
			Name:     SessionCookieName,
			Value:    sess.ID,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Expires:  sess.ExpiresAt,
		})
		response.WriteOK(w, map[string]string{"username": sess.Username})
	}
}

// handleDeleteSession 处理登出。
//
// 销毁服务端会话而非仅清 Cookie：只清 Cookie 的话，
// 已经泄露出去的会话 ID 仍然有效直到自然过期。
func handleDeleteSession(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sess, ok := SessionFrom(r.Context()); ok {
			d.Sessions.Delete(sess.ID)
		}
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure omitted intentionally, same reason as handleCreateSession
			Name:     SessionCookieName,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
		response.WriteOK(w, nil)
	}
}

// handleCurrentSession 返回当前登录身份。
func handleCurrentSession() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := SessionFrom(r.Context())
		if !ok {
			// RequireSession 已经拦过，走到这里说明中间件装配错了。
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeUnauthenticated)
			return
		}
		response.WriteOK(w, map[string]string{"username": sess.Username})
	}
}
