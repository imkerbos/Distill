package registry

import "time"

// AgentState 是一个集群 agent 的状态，封闭枚举。
//
// 只有两态，且没有「暂停」：轮换的做法是签发新的、把旧的置 REVOKED，
// 允许两把 token 并存一段重叠期。多一个中间态就要多回答一次「暂停中的
// token 还能不能推数据」，而那个问题没有安全的答案 —— 答能，暂停就没有
// 意义；答不能，它与吊销就是同一件事。
type AgentState string

const (
	// AgentActive 表示这把 token 现在可以用。
	AgentActive AgentState = "ACTIVE"
	// AgentRevoked 表示这把 token 已被吊销，永不复活。
	//
	// 不提供「取消吊销」：吊销的成因通常是泄漏，而一把泄漏过的 token
	// 不会因为有人后悔就重新变得安全。要恢复采集，签一把新的。
	AgentRevoked AgentState = "REVOKED"
)

// Valid 判断该状态是否已登记。
//
// 用显式 switch 而非查表：新增一个常量却忘了加进这里，switch 让这处
// 遗漏在 review 时是看得见的一行（同 RepoVerifyResult.Valid）。
func (s AgentState) Valid() bool {
	switch s {
	case AgentActive, AgentRevoked:
		return true
	default:
		return false
	}
}

// ClusterAgent 是跑在被管集群里的那个采集器的机器身份
// （design doc 2026-08-18 §3）。
//
// 与 PlatformAccount 完全分开，不是复用（design doc §3.1）：那是人，有
// 密码、有角色、有登录限流、有最后管理员保护。把机器套进那套东西，
// 「吊销一个 agent」与「停用一个人」就会走同一条路径，而两者误操作的
// 后果不是一回事。
//
// 主键是 (ClusterID, AgentID) 而不是 AgentID 单列（CLAUDE.md §4）：这条
// 记录决定一次推送的数据归到哪个集群，归错的后果是 A 集群的 Pod 写进
// B 集群的身份表，之后 join 落到错误的 Pod 上**且不报错**。
type ClusterAgent struct {
	// ClusterID 是这把 token 绑定的集群。
	//
	// **摄入路径上的集群归属只来自这里**，不来自请求体（design doc §2）。
	ClusterID string
	// AgentID 是 token 的公开段，用于定位与吊销。它本身不是秘密。
	AgentID string
	// TokenHash 是 SHA-256(token) 的 32 字节。
	//
	// 明文只在签发那一次的响应里出现，此后平台只有这个哈希（规范 §19、§33）。
	TokenHash []byte
	// State 是当前状态。
	State AgentState
	// CreatedBy 是签发它的操作者登录名。
	CreatedBy string
	// CreatedAt 是签发时刻。
	CreatedAt time.Time
	// LastSeenAt 是最近一次成功认证的时刻；从未用过时为零值。
	//
	// 它是给操作者看的「这个 agent 还活着吗」，不是一条安全判定 ——
	// 写它失败不该拒掉一次合法推送。
	LastSeenAt time.Time
	// RevokedAt 是吊销时刻；未吊销时为零值。
	RevokedAt time.Time
}

// TokenHashLen 是 SHA-256 摘要的字节数。
//
// 导出，因为签发侧（internal/agentauth）与校验侧要对同一个长度达成一致，
// 而两处各写一个 32 会在将来换摘要算法时漏改一处 —— 漏改的症状是比对
// 恒不相等，与「token 错了」一模一样。
const TokenHashLen = 32

// AgentIDLen 是 agent 公开段的十六进制长度。
//
// 平台自己生成的定长 hex。定长判定而不是「非空即可」：这个值会被拼进
// 查询、日志与吊销入口，放宽字符集等于把一个外部可控的字符串接进那些
// 位置，而它本可以是封闭的（规范 §3.1、§15）。
const AgentIDLen = 16
