package registry

// Role 是账号的角色，封闭枚举。
//
// **今天它买不到一条真实的权限边界。** 平台只有一个账号（配置文件里的
// auth.bootstrap_user，见 config.AuthConfig），它是管理员，因此当前不存在
// 两个不同权限的调用方，也就没有任何一次调用会被角色拦下。这个枚举与
// 它撑起的路由声明存在的意义只有两条：
//
//  1. 第二个身份出现时，拦截结构已经在原地，不必回头给一堆端点补声明；
//  2. 新加的端点如果忘了声明所需角色，会被拒绝而不是放行（见
//     internal/httpapi 的路由授权表）。
//
// 不要因为这里有一套角色，就以为已经有人被挡在门外了。
//
// 定义在 internal/registry 而不是 internal/auth：角色是账号的一个属性，
// 账号记录本身就落在这个包里（见 Account），internal/auth 管的是"证明
// 你是谁"，不是"你能做什么"。这条依赖方向也是硬约束——internal/auth
// 校验密码之后要从账号记录里取角色，若 Role 定义在 internal/auth，
// internal/registry 反过来 import 它就会与 internal/auth 未来 import
// internal/registry 的需要形成循环（Go 不允许包循环 import）。
type Role string

const (
	// RoleAdmin 是管理员：可以创建、修改、删除、绑定、导入、覆盖、校验，
	// 以及改动平台设置。引导账号就是它。
	RoleAdmin Role = "ADMIN"
	// RoleViewer 是只读账号：只能读取拓扑、流量、质量、安全与策略预览。
	RoleViewer Role = "VIEWER"
)

// Valid 判断该角色是否已登记。
//
// 显式 switch 而非查表：新增一个取值却没在这里列出来，它就不是一个合法
// 角色，而这一行的缺失在评审里看得见。角色的默认值（空串）不合法 ——
// 一个没被赋过角色的会话必须什么都做不了，而不是继承某个取值的权限。
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleViewer:
		return true
	default:
		return false
	}
}

// Permits 判断持有本角色的调用方是否满足 required 所要求的角色。
//
// 这里刻意写成「按被要求的角色分派」而不是给角色排一个数值序：序意味着
// 「更高的数字自动获得更低者的全部权限」，那条规则一旦写下，后面每加一个
// 角色都要重新论证它插在哪儿，而插错的后果是静默的越权。今天只有一条
// 包含关系需要成立 —— 管理员也能读 —— 于是只写下这一条。
//
// required 不是已登记的取值时返回 false：调用方要求了一个不存在的角色，
// 唯一安全的答复是拒绝。
func (r Role) Permits(required Role) bool {
	switch required {
	case RoleViewer:
		return r == RoleAdmin || r == RoleViewer
	case RoleAdmin:
		return r == RoleAdmin
	default:
		return false
	}
}

// allRoles 是枚举的唯一登记处，供测试遍历。
var allRoles = []Role{RoleAdmin, RoleViewer}

// AllRoles 返回全部已登记的角色。
func AllRoles() []Role {
	out := make([]Role, len(allRoles))
	copy(out, allRoles)
	return out
}
