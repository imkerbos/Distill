package registry

import (
	"errors"
	"time"
	"unicode/utf8"
)

// ErrLastAdmin 表示这次操作会让库里不再有任何一个启用中的管理员，因而
// 被拒绝。
//
// 停用、软删除或把最后一个启用中的管理员降级，平台就再也没有人能管理
// 它——没有人能把它改回来（design doc 2026-08-14 §5）。这条判定必须由
// 存储层在同一个事务里做出，不能先查"还有几个管理员"再写：两个管理员
// 同时把对方降级，先查后写会让两次操作都通过（规范 §25，CLAUDE.md
// 并发一节）。
var ErrLastAdmin = errors.New("cannot remove the last enabled admin account")

// Account 是一条平台账号的读模型：列表与详情接口返回的就是这个类型
// （design doc 2026-08-14 §6）。
//
// 刻意不含密码或密码哈希字段。哈希只应在"写入新哈希""校验旧密码"这类
// 单点动作里以参数形式短暂出现，绝不该挂在一个会被整体序列化进响应体
// 的类型上——把它排除在类型之外，"一次响应把哈希带出去了"就成了这个
// 类型本身表达不出来的状态，而不是每一条新加的返回路径都要有人记得去
// 检查的一条规则（规范 §19、§20；design doc 2026-08-14 §3、§6）。
type Account struct {
	// Username 是账号的登录名，同时是这条记录的主键。
	Username string
	// Role 是账号当前生效的角色。
	//
	// 类型定义在本包（见 role.go），不在 internal/auth：角色是账号的
	// 属性，账号记录本身就落在这个包里；internal/auth 只管"证明你是
	// 谁"，验证密码之后要从这条记录取角色，因此依赖方向只能是
	// internal/auth 依赖 internal/registry，不能反过来
	// （design doc 2026-08-14 现状 1、role.go 的类型注释）。
	Role Role
	// DisabledAt 非空时表示账号处于停用状态。
	//
	// 停用是可逆的暂停，与软删除是两件事：账号被删除后这条记录仍然
	// 保留（用户名不复用），停用只是把它暂时关掉（design doc
	// 2026-08-14 §3）。
	DisabledAt *time.Time
	// CreatedAt 是账号创建时间。
	CreatedAt time.Time
	// UpdatedAt 是账号记录最近一次变更时间。
	UpdatedAt time.Time
}

// ValidateAccount 校验一条账号记录里由调用方提供的字段。
//
// 只看 Username 与 Role：CreatedAt/UpdatedAt/DisabledAt 由存储层在写入
// 时维护，不是调用方要对之负责的输入。
func ValidateAccount(a Account) error {
	if a.Username == "" {
		return invalid("用户名不能为空")
	}
	if !a.Role.Valid() {
		return invalidf("角色 %q 不在已登记的取值范围内", a.Role)
	}
	return nil
}

const (
	// passwordMinRunes 是密码强度下限，按字符（rune）计数（design doc
	// 2026-08-14 §6：「密码最短 12 字符」）。
	//
	// 这里必须按字符而不是字节：平台界面是中文，一个中文字符在 UTF-8
	// 下占 2~4 字节。如果按字节数把关，12 字节相当于四五个汉字就能
	// 达标，远低于"12 个字符"这个设计意图里的输入复杂度。
	passwordMinRunes = 12
	// passwordMaxBytes 是密码长度上限，直接取自 bcrypt 的输入上限：
	// bcrypt 只取密码的前 72 字节参与哈希，之后的内容被静默丢弃，不会
	// 报错、也不会体现在任何地方。
	//
	// 这里必须按字节而不是字符：按字符数把关会放过恰好在这条线上被
	// 截断的密码——中文字符每个占 2~4 字节，一个远不到 72 个字符的
	// 密码可能已经越过 72 字节这条 bcrypt 真正在意的边界，此时若按
	// 字符数放行，最后几个字符会被悄悄丢弃而操作者对此一无所知
	// （task brief「两个属性」之二）。
	passwordMaxBytes = 72
)

// ValidatePassword 校验一个明文密码在被 bcrypt 哈希之前是否合法。
//
// 上限与下限刻意使用两种不同的计数单位（见 passwordMinRunes /
// passwordMaxBytes 的注释）：下限管的是"输入了多少个字符"这件对操作者
// 有意义的事，上限管的是"bcrypt 实际会用到多少字节"这件与算法实现
// 绑定的事，两者不是同一个问题，混用一种单位会在其中一个方向上判错。
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < passwordMinRunes {
		return invalidf("密码长度不能少于 %d 个字符", passwordMinRunes)
	}
	if len(password) > passwordMaxBytes {
		return invalidf("密码长度不能超过 %d 字节：这是 bcrypt 的输入上限，"+
			"超出的部分会被静默丢弃而不会生效，且按字节而非字符计数——"+
			"中文字符在 UTF-8 下占 2~4 字节，一个远不到 %d 个字符的密码"+
			"也可能已经越过这条边界", passwordMaxBytes, passwordMaxBytes)
	}
	return nil
}
