// Package auth 负责账号校验与会话生命周期。
package auth

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/registry"
)

// dummyHash 是一个真实的 bcrypt 哈希，用于在用户不存在时仍走一次比对。
//
// 不这样做的话，未知用户会立即返回，而存在的用户要等一次 bcrypt 计算，
// 响应耗时的差异足以让攻击者枚举出哪些账号真实存在。
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

// AccountSource 是账号记录的读取来源。registry.Store 满足它。
//
// 收窄成两个读方法而不是直接依赖 registry.Store：认证只需要知道「这个
// 账号此刻是什么状态」，而 registry.Store 还带着建号、改角色、停用、
// 软删除的全部写方法 —— 让登录这条路径在类型上具备改写账号表的能力，
// 没有任何理由（与 settings.Source 同一处置）。
type AccountSource interface {
	// Accounts 返回全部未删除的账号。
	Accounts(ctx context.Context) ([]registry.Account, error)
	// Account 按用户名查一个未删除的账号。不存在时第二个返回值为 false。
	Account(ctx context.Context, username string) (registry.Account, bool, error)
	// AccountPasswordHash 读一个此刻能够登录的账号的密码哈希。
	// 账号不存在、已停用或已软删除时第二个返回值为 false。
	AccountPasswordHash(ctx context.Context, username string) (registry.PasswordHash, bool, error)
}

// Verifier 校验登录凭证，并在每次授权判定时现读账号的角色。
//
// **它不缓存任何账号状态。** 角色、停用与软删除都从 accounts 现读：把角色
// 在签发会话时抄进内存，等于让降权与停用等到那张会话过期（默认 8 小时）
// 才生效（design doc 2026-08-14 §4）。
type Verifier struct {
	// bootstrapUser 是配置里的引导账号名。空串表示没有配置引导账号。
	bootstrapUser string
	// bootstrapHash 是引导账号的密码哈希，来自配置文件。
	bootstrapHash []byte
	// accounts 是库里的账号记录。
	accounts AccountSource
}

// NewVerifier 用配置里的引导账号与账号表构造校验器。
//
// 这里不再给账号赋角色 —— 原先那段「文件里的账号一律是管理员」的注释
// 已经预告了这一天：账号落库之后，角色随每条账号记录走（见 RoleOf）。
//
// 参数从 []config.User 收成单个 config.User，是因为文件里本就只可能有
// 引导账号这一个（config.AuthConfig 只有 BootstrapUser 一个字段）。留着
// 切片的形状，等于给「配置文件里再加几个账号」留了一个入口，而那些账号
// 既不在审计里、也不受第 5 节「不得把自己锁在门外」的保护。
func NewVerifier(bootstrap config.User, accounts AccountSource) *Verifier {
	return &Verifier{
		bootstrapUser: bootstrap.Username,
		bootstrapHash: []byte(bootstrap.PasswordHash),
		accounts:      accounts,
	}
}

// Verify 校验用户名与密码。凭证不可用时返回 false，账号表读不出来时返回错误。
//
// 无论用户是否存在都执行一次哈希比对，调用方无法从耗时或返回值区分
// "用户不存在"与"密码错误"。
//
// 引导账号**只在库里没有任何启用中的管理员时**才可能通过：首次启动时它是
// 唯一的入口，第一个库内管理员建出来之后它自己失效，不需要谁记得去关掉
// 它；而全部管理员被停用或删除之后它又自己回来，否则平台就被永久锁死了
// （design doc 2026-08-14 §2）。
//
// 读不到账号表时返回 (false, err)：读失败不是"没有限制"，一次数据库抖动
// 不该变成一次放行（规范 §49）。
func (v *Verifier) Verify(ctx context.Context, username, password string) (bool, error) {
	if !v.isBootstrap(username) {
		hash, found, err := v.accounts.AccountPasswordHash(ctx, username)
		if err != nil {
			return false, fmt.Errorf("read account password hash: %w", err)
		}
		if !found {
			// 账号不存在、已停用或已软删除时也要走完一次哈希比对：直接
			// 返回会让这条分支比"密码错了"那条快一整次 bcrypt 计算，而那
			// 段差异足以把登录接口变成一个用户名探针。
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
			return false, nil
		}
		// Matches 恒定做完一次 bcrypt 计算，与上面那条 dummy 比对同量级，
		// 两条分支因此在耗时上认不出来（见 registry.PasswordHash.Matches）。
		return hash.Matches(password), nil
	}

	available, err := v.bootstrapAvailable(ctx)
	if err != nil {
		return false, err
	}
	if !available {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return false, nil
	}
	// 密码不匹配返回 (false, nil)：那是一次业务级失败，不是系统错误。
	// 错误位只留给"账号表读不出来"这类调用方无从自行处置的情况。
	if bcrypt.CompareHashAndPassword(v.bootstrapHash, []byte(password)) == nil {
		return true, nil
	}
	return false, nil
}

// RoleOf 返回 username 此刻生效的角色。账号不存在、已停用、已软删除，或者
// 记录里的角色不在封闭枚举内时，第二个返回值为 false。
//
// **每一次授权判定都要调它一次，结果不得被缓存进会话。** 会话只携带身份
// （见 Session），角色现读 —— 降权与停用因此在下一次请求就生效，而不是等
// 那张最长 8 小时的会话过期（design doc 2026-08-14 §4）。
//
// 不采用「变更权限时顺手撤销会话」作为替代：那要求每一条会改变权限的代码
// 路径都记得去撤销，漏掉一条就是一个不会报错的提权窗口；现读则是结构上
// 漏不掉的。
//
// 代价是每个已认证请求多一次单行主键读，与设置的按需读取同量级
// （design doc 2026-08-14 §10）。
func (v *Verifier) RoleOf(ctx context.Context, username string) (registry.Role, bool, error) {
	if v.isBootstrap(username) {
		// 引导账号的角色同样是现读的：它手里那张会话必须在第一个库内
		// 管理员建出来的下一次请求就失去管理员身份，而不是继续管理平台
		// 到过期为止。
		available, err := v.bootstrapAvailable(ctx)
		if err != nil {
			return "", false, err
		}
		if !available {
			return "", false, nil
		}
		return registry.RoleAdmin, true, nil
	}

	a, found, err := v.accounts.Account(ctx, username)
	if err != nil {
		return "", false, fmt.Errorf("read account: %w", err)
	}
	// 不存在与已软删除在这里是同一件事：Account 只返回未删除的行，而
	// 一张指向已经不存在的账号的会话本就不该还能做任何事。
	if !found {
		return "", false, nil
	}
	// 停用是可逆的暂停，但在它生效期间与"没有这个账号"等价。
	if a.DisabledAt != nil {
		return "", false, nil
	}
	// 库里的角色未必经过 ValidateAccount：迁移、手工 SQL 都绕得开写入时的
	// 校验。认不出的角色只能拒绝 —— 给它兜一个默认角色就是一次凭空授权。
	if !a.Role.Valid() {
		return "", false, nil
	}
	return a.Role, true, nil
}

// ValidateNewUsername 校验一个准备写进账号表的用户名，与配置里的引导账号
// 撞名时拒绝，返回 registry.ErrInvalid。
//
// **判定放在这里而不是 registry.ValidateAccount 里**：撞名要比对的是配置
// 文件里的 auth.bootstrap_user，而 internal/registry 与 internal/mysqlregistry
// 都刻意不读配置。要把判定放进存储层，就得把引导账号名一路穿过 New(db)
// 传下去 —— 那会让一个纯粹的存储包从此带着一份配置状态，而它今天不带。
// internal/auth 是唯一同时握着"引导账号是谁"（NewVerifier 的入参）和
// "账号表是什么"的地方，判定因此落在 Verifier 上。
//
// 撞名的后果不是不好看，是两件具体的事：
//
//   - **静默失效**：管理员建出一个与引导账号同名的库内账号，它永远登不
//     进来 —— Verify 先看引导分支，撞名的库内账号连读都不会被读到。界面
//     显示建号成功，那个人却谁也不是。
//   - **越权**：RoleOf 同样先看引导分支。一个角色写着 VIEWER 的库内账号，
//     只要引导闸门开着（平台恰好没有启用中的管理员），就会被解析成 ADMIN。
//
// 比对**大小写不敏感**，因为 platform_account 的排序规则是
// utf8mb4_0900_ai_ci：库把 "boot" 与 "BOOT" 当成同一个主键，而 isBootstrap
// 用的是 Go 的 == 。只按 == 拒绝，"BOOT" 就会建成功，然后 Verify("BOOT")
// 走库内分支、Verify("boot") 走引导分支 —— 一个用户名，两套凭证、两套
// 可用时机，正是本条要拒绝的形状。多拒的那些名字是安全方向。
//
// 没有配置引导账号时恒放行：此时不存在引导身份，也就不存在撞名。
func (v *Verifier) ValidateNewUsername(username string) error {
	if v.bootstrapUser == "" {
		return nil
	}
	if strings.EqualFold(username, v.bootstrapUser) {
		return registry.NewInvalidError(
			"用户名与配置文件里的引导账号同名（大小写不敏感），请换一个：" +
				"两套凭证共用一个名字之后，这个账号既登不进来、又可能在引导口" +
				"打开时被当成管理员")
	}
	return nil
}

// isBootstrap 判断 username 是不是配置里的引导账号。
//
// 没有配置引导账号时（空串）恒为假：否则一个空用户名会走进引导分支，
// 而一次装配失误就成了一条管理员通道（规范 §49）。
func (v *Verifier) isBootstrap(username string) bool {
	return v.bootstrapUser != "" && username == v.bootstrapUser
}

// bootstrapAvailable 判断引导账号此刻是否可用：库里没有任何一个启用中的
// 管理员。
//
// 判定只看"启用中的管理员"这一件事。只读账号再多、被停用的管理员再多，
// 都没有人能管理这个平台，逃生口必须还在（design doc 2026-08-14 §2）。
// 软删除的账号不在 Accounts 的返回里，因此不必在这里再判一次。
func (v *Verifier) bootstrapAvailable(ctx context.Context) (bool, error) {
	accounts, err := v.accounts.Accounts(ctx)
	if err != nil {
		return false, fmt.Errorf("read accounts: %w", err)
	}
	for _, a := range accounts {
		if a.Role == registry.RoleAdmin && a.DisabledAt == nil {
			return false, nil
		}
	}
	return true, nil
}
