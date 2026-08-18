// Package policygen 从流量证据与资产快照生成候选 NetworkPolicy。
//
// 生成的策略是"建议"，不是事实：每条规则都带证据来源与风险标记，
// 带风险的规则默认不启用。一个把 SSH 出公网悄悄写进推荐策略的生成器，
// 比没有生成器更糟 —— 它让一次已知敞口获得了平台的背书。
package policygen

import (
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
)

// RuleOrigin 区分规则来源。
type RuleOrigin string

const (
	// OriginBaseline 表示规则来自 Baseline 推导，永不被学习结果覆盖。
	OriginBaseline RuleOrigin = "BASELINE"
	// OriginLearned 表示规则从观测流量学习得出。
	OriginLearned RuleOrigin = "LEARNED"
)

// EvidenceClass 是学习规则的证据来源。封闭枚举。
//
// 分类而非只记"学到了"：同样一条放行规则，来自一次正常的服务调用
// 与来自一次当前被策略拦下的数据库直连，处置方式完全不同。
type EvidenceClass string

const (
	// EvidenceTrustedAllow 是集群内、身份可信、当前放行的流量。
	EvidenceTrustedAllow EvidenceClass = "TRUSTED_ALLOW"
	// EvidenceTrustedDeny 是连接确实发生过、但当前被策略拦下的流量。
	//
	// 学进来意味着把一个当前被拦的连接改成放行，因此永不默认启用。
	EvidenceTrustedDeny EvidenceClass = "TRUSTED_DENY"
	// EvidenceInternetEgress 是出公网流量，只能用 ipBlock 表达。
	EvidenceInternetEgress EvidenceClass = "INTERNET_EGRESS"
	// EvidenceCrossCluster 是跨集群流量，对端只能用 ipBlock 表达且 IP 会漂。
	EvidenceCrossCluster EvidenceClass = "CROSS_CLUSTER"
	// EvidenceIncompleteWindow 是身份可信、但观测窗口证明不了自己没漏的流量。
	//
	// 三种来源没有一条能自证完整：Hubble 报不出采样率与丢弃数，VPC flow logs
	// 不报丢弃，conntrack 轮询天然漏短连接。因此这一类不是例外，是常态
	// （design doc 2026-08-18-learn-from-incomplete-evidence §1）。
	//
	// **它不等于 EvidenceTrustedAllow，因此 Enabled 恒为 false** ——
	// 规则生成出来，但必须有人逐条确认才进生效集。代价写在 §3：漏看的连接
	// 不会进候选，覆盖它的规则于是缺席，而缺席的规则会被判「无流量、可收紧」。
	EvidenceIncompleteWindow EvidenceClass = "INCOMPLETE_WINDOW"
)

// allEvidenceClasses 是枚举的唯一登记处。
var allEvidenceClasses = []EvidenceClass{
	EvidenceTrustedAllow, EvidenceTrustedDeny,
	EvidenceInternetEgress, EvidenceCrossCluster,
	EvidenceIncompleteWindow,
}

// AllEvidenceClasses 返回全部已登记的证据类型。
func AllEvidenceClasses() []EvidenceClass {
	out := make([]EvidenceClass, len(allEvidenceClasses))
	copy(out, allEvidenceClasses)
	return out
}

// Valid 判断该证据类型是否已登记。空值视为合法，表示 Baseline 规则。
func (e EvidenceClass) Valid() bool {
	if e == "" {
		return true
	}
	for _, known := range allEvidenceClasses {
		if e == known {
			return true
		}
	}
	return false
}

// UngeneratableReason 是流量无法表达为规则的原因。封闭枚举。
//
// 与 UnknownReason 同一套纪律：只报"生成了 N 条规则"而不报"有 M 条
// 流量表达不了"，等于宣称覆盖完整。自由文本无法聚合这个缺口。
type UngeneratableReason string

const (
	// ReasonNoWorkloadLabel 表示端点 Pod 没有 app 标签，selector 无法表达。
	ReasonNoWorkloadLabel UngeneratableReason = "NO_WORKLOAD_LABEL"
	// ReasonIdentityUnknown 表示判定为 UNKNOWN，端点身份未还原。
	ReasonIdentityUnknown UngeneratableReason = "IDENTITY_UNKNOWN"
	// ReasonDegradedEvidence 表示存在 mesh 或 CCNP 干扰。
	//
	// spec §6.4 禁止把 DEGRADED 结论作为策略推荐的依据。
	ReasonDegradedEvidence UngeneratableReason = "DEGRADED_EVIDENCE"
	// ReasonUnmanagedEndpoint 表示端点不受 NetworkPolicy 管控，如 hostNetwork Pod。
	ReasonUnmanagedEndpoint UngeneratableReason = "UNMANAGED_ENDPOINT"
	// ReasonLabelKeyConflict 表示主体 Pod 的 workload 归属键不是该
	// (namespace, workload) 的赢家（见 resolveWinningKeys），候选策略的
	// podSelector 选不中它，这条流量因此没有能承载它的规则。
	ReasonLabelKeyConflict UngeneratableReason = "LABEL_KEY_CONFLICT"
)

// allUngeneratableReasons 是枚举的唯一登记处。
var allUngeneratableReasons = []UngeneratableReason{
	ReasonNoWorkloadLabel, ReasonIdentityUnknown,
	ReasonDegradedEvidence, ReasonUnmanagedEndpoint,
	ReasonLabelKeyConflict,
}

// AllUngeneratableReasons 返回全部已登记的不可生成原因。
func AllUngeneratableReasons() []UngeneratableReason {
	out := make([]UngeneratableReason, len(allUngeneratableReasons))
	copy(out, allUngeneratableReasons)
	return out
}

// Valid 判断该原因是否已登记。
func (r UngeneratableReason) Valid() bool {
	for _, known := range allUngeneratableReasons {
		if r == known {
			return true
		}
	}
	return false
}

// UngeneratableItem 是一条无法表达为规则的流量。
type UngeneratableItem struct {
	// FlowID 是流量标识。
	FlowID string `json:"flowId"`
	// Reason 是原因，取值为封闭枚举。
	Reason UngeneratableReason `json:"reason"`
	// Detail 仅用于展示，不参与统计。
	Detail string `json:"detail"`
}

// WorkloadExclusionReason 是 workload 未进入候选策略花名册的原因。封闭枚举。
//
// 与 UngeneratableReason 分开建一套：后者报的是"某条流量表达不了"，
// 前者报的是更早一步的缺口——这个 workload 从未进入名册，因此根本
// 不会出现在任何一条流量的判定里，UngeneratableReason 那条报不出来。
type WorkloadExclusionReason string

const (
	// ExclusionHostNetwork 表示 Pod 使用 hostNetwork，不受 NetworkPolicy 管控。
	ExclusionHostNetwork WorkloadExclusionReason = "UNMANAGED_ENDPOINT"
	// ExclusionNoWorkloadLabel 表示 Pod 不带任何可识别的 workload 标签
	// （见 workloadLabelKeys），podSelector 无法表达。
	ExclusionNoWorkloadLabel WorkloadExclusionReason = "NO_WORKLOAD_LABEL"
	// ExclusionLabelKeyConflict 表示同一个 (namespace, workload) 上另有
	// 优先级更高的归属标签键，这个 Pod 挂在输的那个键上。
	//
	// 它确实没有候选策略：赢家的 podSelector 用赢家的键构造，选不中它。
	// 报出来好过发第二条同名策略——后者不报错，只是永远选不中任何 Pod。
	ExclusionLabelKeyConflict WorkloadExclusionReason = "LABEL_KEY_CONFLICT"
)

// allWorkloadExclusionReasons 是枚举的唯一登记处。
var allWorkloadExclusionReasons = []WorkloadExclusionReason{
	ExclusionHostNetwork, ExclusionNoWorkloadLabel, ExclusionLabelKeyConflict,
}

// AllWorkloadExclusionReasons 返回全部已登记的排除原因。
func AllWorkloadExclusionReasons() []WorkloadExclusionReason {
	out := make([]WorkloadExclusionReason, len(allWorkloadExclusionReasons))
	copy(out, allWorkloadExclusionReasons)
	return out
}

// Valid 判断该原因是否已登记。
func (r WorkloadExclusionReason) Valid() bool {
	for _, known := range allWorkloadExclusionReasons {
		if r == known {
			return true
		}
	}
	return false
}

// ExcludedWorkload 是一个从未进入候选策略花名册的 Pod。
//
// 候选策略按 Pod 名册生成（见 Input.Pods 的注释）；hostNetwork 与无
// workload 标签的 Pod 在名册构建时就被挡在外面。这两类 Pod 因此永远
// 不会作为主体出现在任何一条流量判定里，只报 Ungeneratable 会把它们
// 的存在完全抹掉——"候选策略 4 条，不可生成 0 条"读起来像是覆盖完整，
// 实际上集群里另外 12 个 Pod 根本没进入候选集。
type ExcludedWorkload struct {
	// Namespace 是 Pod 所在命名空间。
	Namespace string `json:"namespace"`
	// Pod 是 Pod 名称。按 Pod 而非按聚合的 workload 计数展示：同一
	// namespace 可能有多个不同原因或不同标签取值的问题 Pod，只报统计
	// 数字找不到该去改哪一个。
	Pod string `json:"pod"`
	// Labels 是该 Pod 的原始标签，供排查是否只是标签键拼错或用了
	// 平台还未识别的键。
	Labels map[string]string `json:"labels"`
	// Reason 是排除原因，取值为封闭枚举。
	Reason WorkloadExclusionReason `json:"reason"`
}

// Observation 是一条带判定结果的观测流量。
type Observation struct {
	// FlowID 是流量标识。
	FlowID string
	// Flow 是流量本身。
	Flow replay.Flow
	// Decision 是该流量在当前策略下的判定。
	Decision replay.Decision
	// IdentityTrusted 是求值引擎对**主体身份**的可信度，与窗口完整度无关。
	//
	// Decision.Confidence 把两件事压成一个值：mesh / CCNP 让 L4 身份本身不可信，
	// 而窗口不完整只是"可能没看全"。前者学出的规则会挂到**错的主体**上，
	// 后者的规则本身没错、只是可能不够（design doc 2026-08-18-learn-from-incomplete-evidence §2）。
	//
	// 由调用方在传导完整度**之前**取，**不从 UnknownReason 反推** —— 那是一次
	// 猜测，而猜错的方向是把 mesh 流量当成可学的证据。
	IdentityTrusted bool
}

// Rule 是候选策略中的一条规则及其来源与风险标注。
type Rule struct {
	// Origin 是规则来源。
	Origin RuleOrigin `json:"origin"`
	// Evidence 是学习证据类型；Origin 为 BASELINE 时为空。
	Evidence EvidenceClass `json:"evidence,omitempty"`
	// Baseline 是 Baseline 类型；Origin 为 LEARNED 时为空。
	Baseline *baseline.Kind `json:"baseline,omitempty"`
	// Derivations 是 Baseline 的推导依据；Origin 为 LEARNED 时为空。
	Derivations []baseline.Derivation `json:"derivations,omitempty"`
	// Risk 在命中风险端口清单时非空。
	//
	// 用指针而非零值：零值端口 0 与"未命中风险清单"在结构上必须可区分，
	// 否则界面会给每条普通规则都渲染一个风险徽标。
	Risk *risk.Port `json:"risk,omitempty"`
	// Enabled 表示该规则是否进入生效策略集。
	Enabled bool `json:"enabled"`
	// Direction 是规则方向。
	Direction replay.Direction `json:"direction"`
	// FlowCount 是支撑该规则的观测流量条数；Baseline 为 0。
	FlowCount int `json:"flowCount"`
	// Peers 是对端的展示视图：selector 对端为 namespace/workload，
	// ipBlock 对端为 CIDR。
	//
	// 与 Ports 一同随规则返回，而不是让消费方去解析 networkingv1 结构：
	// 只报"某 workload 有 4 条启用规则"而不报放行了谁的哪个端口，
	// 读的人无法把 payment:8080 与 0.0.0.0/0:443 区分开。
	Peers []string `json:"peers"`
	// Ports 是端口的展示视图，形如 TCP/8080。
	Ports []string `json:"ports"`
	// Fingerprint 是规则内容的 SHA-256，人工覆盖决定挂在它上面。
	//
	// 只覆盖内容（Origin / Evidence / Direction / Peers / Ports），
	// 不含 FlowCount：流量条数每天都在变，算进去会让每一次重新生成
	// 都作废掉全部人工确认。
	//
	// 指纹变了覆盖自动失效，这是刻意的 —— 内容变了就不是当初被确认
	// 的那一条。用 (namespace, workload, 序号) 作键会出现「确认的是
	// MySQL，重新生成后那个位置变成了 SSH，覆盖仍在」。
	Fingerprint string `json:"fingerprint"`
	// Ingress 在 Direction 为 INGRESS 时非空。
	//
	// 不出 API：k8s 结构体一旦进了响应体，界面就得自己解释 selector
	// 语义，而它与后端迟早各解释一套。对外只给上面两个渲染好的视图。
	Ingress *networkingv1.NetworkPolicyIngressRule `json:"-"`
	// Egress 在 Direction 为 EGRESS 时非空。
	Egress *networkingv1.NetworkPolicyEgressRule `json:"-"`
}

// CandidatePolicy 是一个 (cluster, namespace, workload) 的候选策略。
//
// 以 workload 而非 namespace 为单位：单条 Policy 有独立生命周期
// （spec §12），一条覆盖整个 namespace 的策略会让任一 workload 的
// 规则变更都动全 namespace 的版本，回滚粒度随之变粗。
type CandidatePolicy struct {
	// Cluster 是所属集群。
	Cluster string `json:"cluster"`
	// Namespace 是所属命名空间。
	Namespace string `json:"namespace"`
	// Granularity 是主体粒度。
	//
	// NAMESPACE 粒度下 Workload 与 WorkloadLabelKey 为空，**但那是结果、
	// 不是判据** —— 拿空串当粒度标记会让「这个字段没填」与「这是 namespace
	// 粒度」变成同一个状态（design doc 2026-08-19 §6）。
	Granularity Granularity `json:"granularity"`
	// Workload 是主体 workload，即 podSelector 的标签值。
	Workload string `json:"workload"`
	// WorkloadLabelKey 是 Workload 命中的标签键，见 workloadLabelKeys。
	//
	// 与 Workload 分开存放而不是拼进去：真实集群里 app.kubernetes.io/name、
	// app、k8s-app、component 并存，podSelector 必须用实际命中的键构造——
	// coredns 用 k8s-app 归属，生成的 selector 就必须是 {k8s-app: kube-dns}，
	// 拼成 {app: kube-dns} 是一条集群里没有任何 Pod 会命中的幽灵策略。
	WorkloadLabelKey string `json:"workloadLabelKey"`
	// Rules 是规则列表，确定排序。
	Rules []Rule `json:"rules"`
}
