package registry

import "time"

// RepoVerifyResult 是仓库级只读校验的结论。封闭枚举。
//
// 与路径级的 BindingVerifyResult 拆成两个类型，而不是一个枚举配两套
// 「哪些取值在这一层合法」的约定（design doc 2026-08-13 §3.3）：后者的
// 约定只活在注释与 review 里，迟早会漂，而漂的方向是把一句仓库级的话
// 当成路径级的结论写下去 —— 「认证失败」落在绑定上，读的人会以为那是
// 关于 policyPath 的判断。类型拆开之后，这种写法根本编译不过。
//
// 与「仓库能否保存」完全无关：保存与否是动作结果，可信与否是判断结论，
// 两者合并就是用「能保存」冒充「能下发」（design doc §3.2）。
type RepoVerifyResult string

const (
	// RepoVerifyNotVerified 表示尚未校验过。空值在校验中按这个值处理。
	RepoVerifyNotVerified RepoVerifyResult = "NOT_VERIFIED"
	// RepoVerifyOK 表示凭据可解析、仓库可达、认证通过、分支存在。
	//
	// 不表示「可写入」：可写性只有一次真正的写入才能验证，而校验只做
	// 只读操作（design doc §3.1），不得把两者混说。
	RepoVerifyOK RepoVerifyResult = "OK"
	// RepoVerifyCredentialUnresolved 表示 credentialRef 在 Secret Manager
	// 里取不到 —— 平台侧配置问题。
	RepoVerifyCredentialUnresolved RepoVerifyResult = "CREDENTIAL_UNRESOLVED" //nolint:gosec // G101 false positive: this is an enum label, not a credential.
	// RepoVerifyAuthFailed 表示凭据取到了，但仓库拒绝 —— 仓库侧权限问题。
	//
	// 不与 RepoVerifyCredentialUnresolved 合并：两者的处置人不是同一个人，
	// 合并会让排障的人找错负责人。
	RepoVerifyAuthFailed RepoVerifyResult = "AUTH_FAILED"
	// RepoVerifyRepoUnreachable 表示网络或地址问题，未能到达仓库；
	// 出站超时也归入这一类，不单列取值 —— 从操作者视角处置相同。
	RepoVerifyRepoUnreachable RepoVerifyResult = "REPO_UNREACHABLE"
	// RepoVerifyBranchMissing 表示仓库可达、认证通过，但分支不存在。
	RepoVerifyBranchMissing RepoVerifyResult = "BRANCH_MISSING"
)

// Valid 判断该结论是否已登记。
//
// 用显式 switch 而非 map/slice 查表：新增一个常量却忘了把它加进这里，
// switch 让这处遗漏在 review 时是看得见的一行；换成表驱动，遗漏就是
// 一次无声的编译通过。
func (v RepoVerifyResult) Valid() bool {
	switch v {
	case RepoVerifyNotVerified, RepoVerifyOK, RepoVerifyCredentialUnresolved,
		RepoVerifyAuthFailed, RepoVerifyRepoUnreachable, RepoVerifyBranchMissing:
		return true
	default:
		return false
	}
}

// GitRepo 是一个策略仓库，独立于集群存在（design doc 2026-08-13 §3.1）。
//
// 它在任何集群绑定它之前就存在，标识是 URL 而非网段 —— 这也是它的主键
// 不含 cluster_id 的原因（CLAUDE.md 那条规则的成因是不同集群的 Pod CIDR
// 可能重叠；把 cluster_id 塞进这里等于宣称一个仓库属于某个集群，而这
// 正是本次拆分要消除的东西，详见 design doc §3.1）。
type GitRepo struct {
	// ID 是仓库标识，绑定用它来指向仓库。
	ID string `json:"repoId"`
	// URL 是仓库地址，只接受 SSH 形态（见 ValidateGitRepo）。
	URL string `json:"repoUrl"`
	// Branch 是分支。
	Branch string `json:"branch"`
	// CredentialRef 是 Secret Manager 中凭据的引用。
	//
	// 凭据本身永不入库：一个能从平台数据库 dump 出 Git token 的设计，
	// 等于把 GitOps 的信任根搬进了平台。
	CredentialRef string `json:"credentialRef"`
	// VerifiedAt 是最近一次仓库级只读校验发生的时间；nil 表示从未校验过。
	//
	// 这是历史事实，不是当前状态：轮 4 写回前必须重新校验，不能拿一个
	// 几天前的 OK 当作现在可写（design doc §3.4）。
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
	// VerifyResult 是最近一次仓库级校验的结论。
	VerifyResult RepoVerifyResult `json:"verifyResult"`
}
