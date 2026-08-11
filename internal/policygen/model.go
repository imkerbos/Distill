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
)

// allEvidenceClasses 是枚举的唯一登记处。
var allEvidenceClasses = []EvidenceClass{
	EvidenceTrustedAllow, EvidenceTrustedDeny,
	EvidenceInternetEgress, EvidenceCrossCluster,
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
)

// allUngeneratableReasons 是枚举的唯一登记处。
var allUngeneratableReasons = []UngeneratableReason{
	ReasonNoWorkloadLabel, ReasonIdentityUnknown,
	ReasonDegradedEvidence, ReasonUnmanagedEndpoint,
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

// Observation 是一条带判定结果的观测流量。
type Observation struct {
	// FlowID 是流量标识。
	FlowID string
	// Flow 是流量本身。
	Flow replay.Flow
	// Decision 是该流量在当前策略下的判定。
	Decision replay.Decision
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
	// Workload 是主体 workload，即 podSelector 的 app 标签值。
	Workload string `json:"workload"`
	// Rules 是规则列表，确定排序。
	Rules []Rule `json:"rules"`
}
