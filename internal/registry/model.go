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

// DataSource 说明一个集群的读取数据来自哪里。封闭枚举。
//
// **显式登记，不由「有没有数据」推断**（design doc 2026-08-17 §2）。推断的
// 后果是：一次采集故障会让一个真集群悄悄变回演示集群，而页面上完全看不出来
// —— 操作者读到一份写着真集群名字的完整报告，据此批准一次下发，而那些数字
// 描述的是一个虚构的集群。没有任何症状会暴露它。
//
// 也正因为如此，这个字段**不在集群的写路径上**：把一个真集群改成 FIXTURE
// 恰好是上面那个最坏结果的手动版本，而平台目前没有任何针对它的授权与审计
// 设计（规范 §7、§28）。取值由数据库登记 —— 新注册的集群走列默认值
// COLLECTED，两个演示集群由 000014 迁移点名置为 FIXTURE。
type DataSource string

const (
	// DataSourceFixture 表示读 internal/fixture 的合成数据集。
	//
	// 只有种子里的两个演示集群是它：fixture 是这个仓库里唯一一份完整、
	// 可控、已知答案的数据集，判定层的回归基准压在它上面（design doc §6）。
	DataSourceFixture DataSource = "FIXTURE"
	// DataSourceCollected 表示读该集群真实采集到的资产与流量。
	//
	// 没有可用采集时的正确答案是「没有数据」，不是回退到合成数据。
	DataSourceCollected DataSource = "COLLECTED"
)

// Valid 判断该来源是否已登记。
//
// 空串不合法：一个没有登记来源的集群不该被装配出任何 Reader。用显式
// switch 而非查表，理由同 BindingVerifyResult.Valid —— 新增取值却忘了加
// 分支，在 review 里必须是看得见的一行。
func (s DataSource) Valid() bool {
	switch s {
	case DataSourceFixture, DataSourceCollected:
		return true
	default:
		return false
	}
}

// AllDataSources 返回全部已登记的数据来源。
func AllDataSources() []DataSource {
	return []DataSource{DataSourceFixture, DataSourceCollected}
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
	// KubeconfigRef 是 Secret Manager 中该集群 kubeconfig 的引用
	// （design doc 2026-08-16 §3.5）。
	//
	// 存的是短名，**kubeconfig 本身永不入库** —— 形状与取舍都与
	// GitRepo.CredentialRef 一致：一个能从平台数据库 dump 里取出集群凭据的
	// 设计，等于把整个 fleet 的信任根搬进平台。这里的值比 Git 那把更重
	// ——kubeconfig 里带的是能对 apiserver 说话的身份，不是一个仓库的读权限。
	//
	// **只有采集器解析它。** distill-api 存它、显示它，但不得把它交给
	// secrets.Resolver：CLAUDE.md 禁止平台主服务持有日常 Kubernetes 权限，
	// spec §1 把采集器拆成独立二进制正是为了让这件事成为部署事实。
	// 绊线见 internal/registry 的 kubecred_callsite_test.go，那里也写了
	// 它拦不住什么。
	KubeconfigRef string `json:"kubeconfigRef"`
	// State 是接入状态。
	State OnboardState `json:"state"`
	// DataSource 是这个集群的读取数据来源，**只读**。
	//
	// 由数据库登记，读路径填充；CreateCluster / UpdateCluster 一律不落它
	// （见 mysqlregistry/cluster.go）。这里带着一个值也不会被写进去 ——
	// 与 Git 那个字段同一条纪律、同一个理由：一条能绕开正规写入口的路径，
	// 会让「谁把这个集群改成了演示数据」这个问题事后答不出来。
	//
	// 出现在 JSON 里是必须的：界面上必须标出数据来源，一份演示数据与一份
	// 真实报告长得一样是不可接受的（design doc 2026-08-17 §2）。
	DataSource DataSource `json:"dataSource"`
	// APIServers 是 API server 端点。
	APIServers []APIServer `json:"apiServers"`
	// HealthCheckSources 是负载均衡健康检查的来源网段。
	HealthCheckSources []string `json:"healthCheckSources"`
	// NodeAgents 是需要被放行的节点级 agent。
	//
	// 与 MetricsScrapers 同源：agent 连不连工作负载、连哪个端口只有人知道
	// （design doc 2026-08-18-node-agent-applicability §3）。
	NodeAgents []NodeAgentRegistration `json:"nodeAgents"`
	// NoNodeAgentsReason 非空表示操作者**显式声明**这个集群没有需要放行的
	// 节点 agent，NODE_AGENT 这一类因此"不适用"而非"缺失"。
	//
	// 存理由而不是一个布尔：这是一次有后果的判断 —— 判错的方向是监控在
	// 下发之后静默中断。事后要答得出「当初凭什么说不需要」，一个 true
	// 答不出来。
	//
	// 与 NodeAgents 互斥：同时给出两者是一次自相矛盾的登记，收下之后没有
	// 任何一屏知道该信哪一半（见 ValidateCluster）。
	NoNodeAgentsReason string `json:"noNodeAgentsReason,omitempty"`
	// MetricsScrapers 是这个集群里的 metrics 抓取端。
	//
	// 与 HealthCheckSources 并列、理由同源：它是 METRICS_SCRAPE Baseline
	// 依据的一半，而这一半观测不出来（design doc 2026-08-18 §3）。
	MetricsScrapers []MetricsScraper `json:"metricsScrapers"`
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
