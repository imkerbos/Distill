package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// accountView 是账号的响应形状。
//
// 与 registry.Account 分开写，理由与 settingView 同：响应类型里**根本没有**
// 承载密码或哈希的字段，于是"某条返回路径把哈希带出去了"在这一层写不出来，
// 而不是每加一条返回路径都要有人记得去检查（规范 §19、§20、§35；
// design doc 2026-08-14 §6）。registry.Account 本身也没有那个字段，两层
// 各挡一次不是重复：少掉任何一层，另一层就成了唯一那道，而它会被改。
type accountView struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	// DisabledAt 为空表示账号处于启用状态。
	//
	// 回时间点而不是一个 disabled 布尔：停用是可逆的暂停，而"什么时候被
	// 停的"是操作者要看的信息，布尔答不出来。
	DisabledAt *time.Time `json:"disabledAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// toAccountView 把一条账号记录转成响应形状。
func toAccountView(a registry.Account) accountView {
	return accountView{
		Username:   a.Username,
		Role:       string(a.Role),
		DisabledAt: a.DisabledAt,
		CreatedAt:  a.CreatedAt,
		UpdatedAt:  a.UpdatedAt,
	}
}

// createAccountRequest 是新建账号的请求体。
//
// **没有 role 字段，这是刻意的**（design doc 2026-08-14 §6：role 只作为
// 「要改成什么」出现在改角色那一个端点）。新账号一律建成只读，要成为
// 管理员得再走一次 PUT /accounts/{username}/role —— 提权因此永远是一次
// 单独的、有自己审计动作（UPDATE_ACCOUNT_ROLE）的操作，而不是藏在一次
// 建号里的副作用（规范 §7、§28）。
//
// 不是"收下再忽略"：一个永远不会被采纳的字段留在请求形状里，只会让下一个
// 调用方以为自己建出了管理员 —— 沿用 gitRepoPayload 对结论字段的处置。
type createAccountRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// updateRoleRequest 是改角色的请求体。整个平台只有这一处从请求体读 role。
type updateRoleRequest struct {
	Role string `json:"role"`
}

// resetPasswordRequest 是管理员重置他人密码的请求体。
//
// 不含当前密码：管理员并不知道对方的密码，要求他提供等于把这个功能变成
// 一个做不到的操作。它是一次管理员动作，由审计（RESET_PASSWORD）负责
// 事后可查（design doc §6）。
type resetPasswordRequest struct {
	Password string `json:"password"`
}

// changeOwnPasswordRequest 是本人改自己密码的请求体。
//
// 必须带当前密码（规范 §28）：会话可能是一台没锁屏的机器留下的，而改密码
// 会把账号的控制权永久转移给改的人。
type changeOwnPasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// hashPassword 把明文密码哈希成可以落库的形式。
//
// 先校验再哈希：bcrypt 只取前 72 字节，超长的密码会被静默截断而没有任何
// 报错（见 registry.ValidatePassword）。校验的错误是 registry.InvalidError，
// 顺着既有通道回成一句调用方能据以行动的话。
//
// 明文只在这个函数里存在，返回的是哈希：它不进日志、不进响应、不进审计
// 的前后值（规范 §19、§21）。错误一律自造，不包 bcrypt 的原文 —— 那串
// 文本会一路进日志，而它带着密码长度这类不必要的信息。
func hashPassword(password string) (string, error) {
	if err := registry.ValidatePassword(password); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", errors.New("hash password failed")
	}
	return string(hash), nil
}

// decodeAccountRequest 解析账号端点的请求体。
//
// 解析失败是协议层的问题，与集群、仓库、设置端点同一处置：真实的 400，
// 不是 200 + code。
func decodeAccountRequest[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var req T
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
		return req, false
	}
	return req, true
}

// handleListAccounts 返回全部未删除的账号。
func handleListAccounts(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := d.Registry.Accounts(r.Context())
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		out := make([]accountView, 0, len(list))
		for _, a := range list {
			out = append(out, toAccountView(a))
		}
		response.WriteOK(w, out)
	}
}

// handleCreateAccount 新建一个账号。
//
// 用户名先过 ValidateNewUsername：与配置里的引导账号撞名的账号既登不进来、
// 又会在引导闸门开着时被解析成管理员（见该方法的注释）。这条判定只有
// 校验器做得了 —— 存储层刻意不读配置。
//
// 角色固定为只读，不从请求体读（见 createAccountRequest）。
func handleCreateAccount(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeAccountRequest[createAccountRequest](w, r)
		if !ok {
			return
		}
		if err := d.Verifier.ValidateNewUsername(req.Username); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		hash, err := hashPassword(req.Password)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		account := registry.Account{Username: req.Username, Role: registry.RoleViewer}
		if err := d.Registry.CreateAccount(r.Context(), actorOf(r), account, hash); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 回的是刚建出来的那个身份，不回明文密码（design doc §6）：调用方
		// 自己刚提交过它，回显只会让它多出现在一处日志与一次浏览器缓存里。
		response.WriteOK(w, map[string]string{
			"username": account.Username,
			"role":     string(account.Role),
		})
	}
}

// handleUpdateAccountRole 改一个账号的角色。
//
// 用户名取自路径，角色取自请求体：这是全平台唯一一处从请求体读 role 的
// 地方，而它读到的值是"要改成什么"，不是"我是什么"（规范 §9、§34）。
//
// 降级最后一个启用中的管理员由 UpdateAccountRole 在同一事务内拒绝，
// 不在这里先查后改：两个管理员同时把对方降级，先查后改会双双通过
// （design doc §5，规范 §25）。
func handleUpdateAccountRole(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeAccountRequest[updateRoleRequest](w, r)
		if !ok {
			return
		}
		username := chi.URLParam(r, "username")
		err := d.Registry.UpdateAccountRole(r.Context(), actorOf(r), username, registry.Role(req.Role))
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"username": username, "role": req.Role})
	}
}

// handleDisableAccount 停用一个账号。
//
// 停用不撤销该账号已签发的会话，也不需要：角色在每次请求现读，停用在
// 下一次请求就生效（design doc §4）。
func handleDisableAccount(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := chi.URLParam(r, "username")
		if err := d.Registry.DisableAccount(r.Context(), actorOf(r), username); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"username": username})
	}
}

// handleEnableAccount 重新启用一个账号。
func handleEnableAccount(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := chi.URLParam(r, "username")
		if err := d.Registry.EnableAccount(r.Context(), actorOf(r), username); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"username": username})
	}
}

// handleDeleteAccount 软删除一个账号。
//
// 软删除而非物理删除：审计行记录着这个账号做过什么，而用户名不复用
// 是为了不让新账号继承旧账号在审计里的身份（design doc §3）。
func handleDeleteAccount(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := chi.URLParam(r, "username")
		if err := d.Registry.SoftDeleteAccount(r.Context(), actorOf(r), username); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"username": username})
	}
}

// handleResetAccountPassword 由管理员重置他人的密码。
//
// 不校验当前密码（管理员不知道它），但写审计：动作由存储层按 actor 与目标
// 是否同一个人推出来，调用方不传这个标志 —— 让被审计的一方决定自己那条
// 审计行怎么写，等于没有审计（见 Store.SetAccountPassword）。
//
// 响应里只有用户名：新密码由调用方自己提交，回显它只会让它多留一份痕迹。
func handleResetAccountPassword(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeAccountRequest[resetPasswordRequest](w, r)
		if !ok {
			return
		}
		hash, err := hashPassword(req.Password)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		username := chi.URLParam(r, "username")
		if err := d.Registry.SetAccountPassword(r.Context(), actorOf(r), username, hash); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"username": username})
	}
}

// handleChangeOwnPassword 让任何有效会话改自己的密码。
//
// 目标账号取自会话，不取自请求体或路径：这个端点的授权声明只要求"有一个
// 有效会话"，若目标由调用方指定，一个只读账号就能改管理员的密码
// （规范 §7、§9）。
//
// **必须先校验当前密码**（规范 §28）。校验走 Verifier.Verify 而不是自己
// 取哈希比对：全平台只有那一条读得到哈希的路径，而它把哈希包在一个只答
// 布尔的类型里（registry.PasswordHash）。
func handleChangeOwnPassword(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeAccountRequest[changeOwnPasswordRequest](w, r)
		if !ok {
			return
		}
		sess, ok := SessionFrom(r.Context())
		if !ok {
			// RequireSession 已经拦过，走到这里说明中间件装配错了。
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeUnauthenticated)
			return
		}

		verified, err := d.Verifier.Verify(r.Context(), sess.Username, req.CurrentPassword)
		if err != nil {
			d.Logger.Error("verify current password failed",
				"request_id", RequestIDFrom(r.Context()), "error", err)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}
		if !verified {
			// 与登录同一个码：当前密码不对是一次业务级失败，且这里不需要
			// 比"不正确"更细的说明。错误里不出现密码本身的任何信息。
			response.WriteBusiness(w, response.CodeInvalidCredentials)
			return
		}

		// 新旧密码相同时不拦：这不是一次安全事件，而多一条判定就多一句
		// 回给调用方的"你的新密码和旧的一样"，那句话本身泄露了一次比对
		// 结果。审计仍然记下这次变更发生过。
		hash, err := hashPassword(req.NewPassword)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		err = d.Registry.SetAccountPassword(r.Context(), actorOf(r), sess.Username, hash)
		if errors.Is(err, registry.ErrNotFound) {
			// 走到这里说明当前会话属于引导账号：它的凭据在配置文件里，
			// 账号表里没有对应的行。回一句说得清的话，而不是一个"资源
			// 不存在"——操作者会以为是自己点错了地方。
			response.WriteInvalid(w,
				"引导账号的密码在配置文件里，不能从这里修改；请先创建一个库内管理员账号")
			return
		}
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"username": sess.Username})
	}
}
