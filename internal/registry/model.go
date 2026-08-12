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

// GitBinding 是集群与其 GitOps 仓库的绑定。
//
// 一个不知道自己策略在哪个仓库哪个路径的集群，接入是不完整的 ——
// Git 是策略的部署事实来源。
type GitBinding struct {
	// RepoURL 是仓库地址。
	RepoURL string `json:"repoUrl"`
	// Branch 是分支。
	Branch string `json:"branch"`
	// PolicyPath 是该集群策略在仓库中的根路径。
	PolicyPath string `json:"policyPath"`
	// CredentialRef 是 Secret Manager 中凭据的引用。
	//
	// 凭据本身永不入库：一个能从平台数据库 dump 出 Git token 的设计，
	// 等于把 GitOps 的信任根搬进了平台。
	CredentialRef string `json:"credentialRef"`
	// LastWrittenCommit 是平台最近一次写入该仓库的 commit，漂移检测的基准。
	LastWrittenCommit string `json:"lastWrittenCommit"`
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
