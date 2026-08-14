package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/imkerbos/Distill/internal/registry"
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

		ok, err := d.Verifier.Verify(r.Context(), req.Username, req.Password)
		if err != nil {
			// 账号表读不出来时校验器返回错误而不是 false：读失败不是
			// "凭证不对"，把它答成 10001 会让一次数据库故障看起来像
			// 全公司同时忘了密码。真实原因带 request_id 进日志。
			d.Logger.Error("verify credentials failed",
				"request_id", RequestIDFrom(r.Context()), "error", err)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}
		if !ok {
			// 用户不存在与密码错误返回完全相同的响应，避免账号枚举。
			response.WriteBusiness(w, response.CodeInvalidCredentials)
			return
		}

		if err := auditBootstrapLogin(r.Context(), d, req.Username); err != nil {
			d.Logger.Error("record bootstrap login failed",
				"request_id", RequestIDFrom(r.Context()), "error", err)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}

		// 会话只携带身份，不携带角色：角色在每次授权判定时从账号记录现读
		// （见 authorizer.enforce）。签发时抄一份的话，降权与停用要等这张
		// 会话过期才生效（design doc 2026-08-14 §4）。
		sess, err := d.Sessions.Create(req.Username)
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

// auditBootstrapLogin 在这次登录用的是引导账号时写一条 BOOTSTRAP_LOGIN
// 审计行（design doc 2026-08-14 §2）。
//
// **「是不是引导账号」由账号表反推，而不是由请求体自称。** 走到这里说明
// Verify 刚刚通过，而库内账号只可能经由 AccountPasswordHash 通过，那条读
// 要求 deleted_at 与 disabled_at 都为空 —— 因此"刚认证成功、却在账号表里
// 查不到"只剩引导分支这一种可能。
//
// 失败时把整次登录一起失败掉，而不是记一条日志放行：引导账号能登进来，
// 意味着平台此刻一个启用中的管理员都不剩，那是一次必须在事后可见的事故
// （规范 §43）。一次"没有留下痕迹的逃生口登录"比一次登录失败严重得多。
func auditBootstrapLogin(ctx context.Context, d Deps, username string) error {
	_, found, err := d.Registry.Account(ctx, username)
	if err != nil {
		return fmt.Errorf("read account after sign-in: %w", err)
	}
	if found {
		return nil
	}
	return d.Registry.RecordBootstrapLogin(ctx, registry.Actor{Username: username})
}

// currentSessionView 是当前会话的响应形状。
//
// 带上角色是为了让界面知道该不该渲染管理员入口（design doc §6、§8）。
// 那只是体验：服务端已经在 authorizer.enforce 上拒绝过一次，界面隐藏
// 不构成任何安全边界（规范 §34）。
type currentSessionView struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// handleCurrentSession 返回当前登录身份与它此刻生效的角色。
func handleCurrentSession() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := SessionFrom(r.Context())
		if !ok {
			// RequireSession 已经拦过，走到这里说明中间件装配错了。
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeUnauthenticated)
			return
		}
		// 角色取本次请求已经判定过的那一个，不再自己读一次账号表：两次读
		// 之间账号可能刚被改角色，那样这个端点会回一个与刚才那次授权判定
		// 不一致的答案。缺失同样只可能是中间件装错位置（enforce 没跑）。
		role, ok := roleFrom(r.Context())
		if !ok {
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeUnauthenticated)
			return
		}
		response.WriteOK(w, currentSessionView{Username: sess.Username, Role: string(role)})
	}
}
