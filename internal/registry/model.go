// Package registry 定义集群注册与策略导入的纯类型。
//
// 零 I/O、零框架依赖：集群的校验规则与枚举语义必须能在没有数据库的
// 情况下被完整测试，否则每次改一条校验都要起一个 MySQL。
package registry

import (
	"time"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// OnboardState 是集群的接入状态。封闭枚举。
//
// 不由人手工设置：它反映的是平台实际收到了什么，而不是操作者的意愿。
// 允许手工置为 READY，等于允许把「还没有数据」标成「可以出推荐了」。
type OnboardState string

const (
	// StateRegistered 表示已登记，尚未收到流量。
	StateRegistered OnboardState = "REGISTERED"
	// StateObserving 表示已开始收到流量，学习窗口进行中。
	StateObserving OnboardState = "OBSERVING"
	// StateReady 表示学习窗口足够长，可以产出候选策略。
	StateReady OnboardState = "READY"
)

var allOnboardStates = []OnboardState{StateRegistered, StateObserving, StateReady}

// AllOnboardStates 返回全部已登记的接入状态。
func AllOnboardStates() []OnboardState {
	out := make([]OnboardState, len(allOnboardStates))
	copy(out, allOnboardStates)
	return out
}

// Valid 判断该状态是否已登记。
func (s OnboardState) Valid() bool {
	for _, known := range allOnboardStates {
		if s == known {
			return true
		}
	}
	return false
}

// ImportRole 决定一条导入的策略进 dry-run 的哪一侧。封闭枚举。
type ImportRole string

const (
	// RoleBaselineCurrent 是现状，进 current 侧。
	RoleBaselineCurrent ImportRole = "BASELINE_CURRENT"
	// RoleCandidateAddition 是人工补充的候选规则，进 predicted 侧。
	RoleCandidateAddition ImportRole = "CANDIDATE_ADDITION"
)

var allImportRoles = []ImportRole{RoleBaselineCurrent, RoleCandidateAddition}

// AllImportRoles 返回全部已登记的导入角色。
func AllImportRoles() []ImportRole {
	out := make([]ImportRole, len(allImportRoles))
	copy(out, allImportRoles)
	return out
}

// Valid 判断该角色是否已登记。
func (r ImportRole) Valid() bool {
	for _, known := range allImportRoles {
		if r == known {
			return true
		}
	}
	return false
}

// ImportSource 是策略的传输来源。封闭枚举。
//
// 三者只是传输方式，解析、校验、溯源完全共用；区分它们是为了让
// 「这条现状有没有经过 Git 核对」在界面上可见。
type ImportSource string

const (
	// SourcePaste 是人工粘贴。
	SourcePaste ImportSource = "PASTE"
	// SourceGit 是从绑定的 Git 仓库同步。
	SourceGit ImportSource = "GIT"
	// SourceCluster 是从集群抓取。
	SourceCluster ImportSource = "CLUSTER"
)

var allImportSources = []ImportSource{SourcePaste, SourceGit, SourceCluster}

// AllImportSources 返回全部已登记的导入来源。
func AllImportSources() []ImportSource {
	out := make([]ImportSource, len(allImportSources))
	copy(out, allImportSources)
	return out
}

// Valid 判断该来源是否已登记。
func (s ImportSource) Valid() bool {
	for _, known := range allImportSources {
		if s == known {
			return true
		}
	}
	return false
}

// APIServer 是集群 API server 的访问端点。
type APIServer struct {
	// Host 是端点地址，仅供展示与追溯。
	Host string `json:"host"`
	// CIDR 是端点所在网段，control-plane Baseline 取它。
	CIDR string `json:"cidr"`
	// Port 是访问端口。
	Port int32 `json:"port"`
}

// BindingVerifyResult 是绑定路径级只读校验的结论。封闭枚举。
//
// 与仓库级的 RepoVerifyResult 是两个类型（design doc 2026-08-13 §3.3）：
// 拆分的理由写在 RepoVerifyResult 上。这一层只回答一个问题 ——
// policyPath 在仓库的那个分支上是否存在。
//
// 与「绑定能否保存」完全无关：保存与否是动作结果，可信与否是判断
// 结论，两者合并就是用「能保存」冒充「能下发」（design doc §3.2）。
type BindingVerifyResult string

const (
	// BindingVerifyNotVerified 表示尚未校验过。空值在校验中按这个值处理。
	//
	// 仓库级不是 OK 时，路径级也只能落在这个值上：仓库都没到达过就说
	// 「路径不存在」，是一句没有东西支撑的结论，而操作者会照着它去改
	// policyPath（design doc §3.3）。类型只挡得住把 AUTH_FAILED 写进这一
	// 层，挡不住「跳过仓库级直接报 PATH_MISSING」—— 那一条要由实现路径级
	// 校验器的任务落实：仓库级结论不是 RepoVerifyOK 时必须返回本值。
	BindingVerifyNotVerified BindingVerifyResult = "NOT_VERIFIED"
	// BindingVerifyOK 表示 policyPath 在该分支上存在。
	//
	// 只表示「存在」，不表示「可写入」：可写性只有一次真正的写入才能
	// 验证，而校验只做只读操作（design doc §3.1），不得把两者混说。
	BindingVerifyOK BindingVerifyResult = "OK"
	// BindingVerifyPathMissing 表示仓库级校验已经通过，但 policyPath
	// 在该分支上不存在。
	//
	// 「仓库级已经通过」是这个取值成立的前提，不是背景说明。
	BindingVerifyPathMissing BindingVerifyResult = "PATH_MISSING"
)

// Valid 判断该结论是否已登记。
//
// 用显式 switch 而非 map/slice 查表：新增一个常量却忘了把它加进这里，
// switch 让这处遗漏在 review 时是看得见的一行；换成表驱动，遗漏就是
// 一次无声的编译通过。
func (v BindingVerifyResult) Valid() bool {
	switch v {
	case BindingVerifyNotVerified, BindingVerifyOK, BindingVerifyPathMissing:
		return true
	default:
		return false
	}
}

// GitBinding 是集群与一个已存在的策略仓库的绑定。
//
// 一个不知道自己策略在哪个仓库哪个路径的集群，接入是不完整的 ——
// Git 是策略的部署事实来源。
//
// 仓库地址、分支、凭据不在这里：它们属于 GitRepo（design doc §3.2）。
// 绑定只携带「指向哪个仓库」「策略在仓库里的哪个路径」，以及这个路径
// 自己的校验结论。
type GitBinding struct {
	// RepoID 是被绑定的仓库标识，指向一条 GitRepo。
	RepoID string `json:"repoId"`
	// PolicyPath 是该集群策略在仓库中的根路径。
	PolicyPath string `json:"policyPath"`
	// LastWrittenCommit 是平台最近一次写入该仓库的 commit，漂移检测的基准。
	LastWrittenCommit string `json:"lastWrittenCommit"`
	// VerifiedAt 是最近一次路径级只读校验发生的时间；nil 表示从未校验过。
	//
	// 这是历史事实，不是当前状态：轮 4 写回前必须重新校验，不能拿一个
	// 几天前的 OK 当作现在可写（design doc §3.4）。
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
	// VerifyResult 是最近一次路径级校验的结论。
	VerifyResult BindingVerifyResult `json:"verifyResult"`
}

// Cluster 是一个已注册的集群。
type Cluster struct {
	// ID 是集群标识，全平台的身份主键。
	ID string `json:"id"`
	// DisplayName 是展示名。
	DisplayName string `json:"displayName"`
	// PodCIDR 是 Pod 网段。
	PodCIDR string `json:"podCidr"`
	// NodeCIDR 是节点网段，节点级 Baseline 取它。
	NodeCIDR string `json:"nodeCidr"`
	// CCNPPresent 表示集群存在 Cilium 策略，判定需降级。
	CCNPPresent bool `json:"ccnpPresent"`
	// State 是接入状态。
	State OnboardState `json:"state"`
	// APIServers 是 API server 端点。
	APIServers []APIServer `json:"apiServers"`
	// HealthCheckSources 是负载均衡健康检查的来源网段。
	HealthCheckSources []string `json:"healthCheckSources"`
	// Git 是 GitOps 仓库绑定；未绑定时为 nil。
	Git *GitBinding `json:"git,omitempty"`
}

// ToSnapshot 把注册信息转成 Baseline 推导所需的快照视图。
//
// 这是 registry 与既有推导逻辑之间的唯一桥梁：推导层只认
// snapshot.ClusterRegistry，不知道数据来自配置文件还是数据库。
func (c Cluster) ToSnapshot() snapshot.ClusterRegistry {
	sources := make([]string, len(c.HealthCheckSources))
	copy(sources, c.HealthCheckSources)
	return snapshot.ClusterRegistry{
		ClusterID:          c.ID,
		PodCIDR:            c.PodCIDR,
		NodeCIDR:           c.NodeCIDR,
		HealthCheckSources: sources,
	}
}

// APIServerSnapshots 把注册的端点转成快照视图。
func (c Cluster) APIServerSnapshots() []snapshot.APIServerEndpoint {
	out := make([]snapshot.APIServerEndpoint, 0, len(c.APIServers))
	for _, a := range c.APIServers {
		out = append(out, snapshot.APIServerEndpoint{
			ClusterID: c.ID, Host: a.Host, CIDR: a.CIDR, Port: a.Port,
		})
	}
	return out
}

// PolicyImport 是一条导入的策略。
type PolicyImport struct {
	// ClusterID 是所属集群。
	ClusterID string `json:"clusterId"`
	// ImportID 是导入标识。
	ImportID string `json:"importId"`
	// Plane 当前取值仅 networkpolicy。
	Plane string `json:"plane"`
	// Role 决定它进 dry-run 的哪一侧。
	Role ImportRole `json:"role"`
	// Source 是传输来源。
	Source ImportSource `json:"source"`
	// Namespace 与 Name 来自 YAML 的 metadata。
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// YAML 是原始文本，整段保留 —— 事故复盘时「当初导入的到底是什么」
	// 比解析结果更重要。
	YAML string `json:"yaml"`
	// SpecHash 是 spec 部分的 SHA-256，用于识别内容是否变过。
	SpecHash string `json:"specHash"`
	// GitCommitSHA 在 Source 为 GIT 时非空。为空表示这条现状未经 Git 核对。
	GitCommitSHA string `json:"gitCommitSha"`
	// ImportedBy 与 ImportedAt 是溯源信息。
	ImportedBy string    `json:"importedBy"`
	ImportedAt time.Time `json:"importedAt"`
}

// VerifiedAgainstGit 报告这条导入是否有 Git commit 佐证。
//
// 界面必须据此标注：没有 commit 就无法证明它与仓库里的内容一致，
// 把引导输入与 Git 同步结果显示成同一种东西会高估现状的可信度。
func (p PolicyImport) VerifiedAgainstGit() bool {
	return p.Source == SourceGit && p.GitCommitSHA != ""
}
