// Package baseline 从资产快照推导必备的 Baseline 策略规则。
//
// 每条规则必须带推导依据（spec §7.2）：硬编码的网段常量表不会随网段
// 变化更新，也说不清当初是怎么来的，最终演变为无人敢删的祖传配置。
package baseline

import (
	"errors"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/replay"
)

// Kind 是必备 Baseline 的封闭枚举（spec §7.3 五类）。
type Kind string

const (
	// KindDNS 是 DNS 出向，全部 namespace 必备。
	KindDNS Kind = "DNS"
	// KindLBHealth 是负载均衡健康检查入向。
	KindLBHealth Kind = "LB_HEALTH_CHECK"
	// KindMetrics 是 metrics 抓取入向。
	KindMetrics Kind = "METRICS_SCRAPE"
	// KindControlPlane 是 kube-apiserver 出向。
	KindControlPlane Kind = "CONTROL_PLANE"
	// KindNodeAgent 是节点级 agent 入向。
	KindNodeAgent Kind = "NODE_AGENT"
	// KindExposedIngress 是暴露型 Service 的入站放行。
	//
	// LoadBalancer / NodePort 声明的对外暴露，其入站来源**学不出来**：
	// UAT 上 istio 入口网关一小时内有 4440 个源地址，那是一份每天都在变的
	// /32 清单。不生成它的后果是入口网关拿到一份零放行的 default-deny，
	// 应用即切断全集群外部入口（design doc 2026-08-28 §1）。
	KindExposedIngress Kind = "EXPOSED_INGRESS"
)

// allKinds 是枚举的唯一登记处。新增类型必须同步登记，
// 否则 Valid 会拒绝它，且 Missing 不会把它算进齐备性校验。
var allKinds = []Kind{
	KindDNS, KindLBHealth, KindMetrics, KindControlPlane, KindNodeAgent,
	KindExposedIngress,
}

// AllKinds 返回全部已登记的必备 Baseline 类型。
func AllKinds() []Kind {
	out := make([]Kind, len(allKinds))
	copy(out, allKinds)
	return out
}

// Valid 判断该类型是否已登记。
func (k Kind) Valid() bool {
	for _, known := range allKinds {
		if k == known {
			return true
		}
	}
	return false
}

// SourceKind 是推导依据的来源类型。封闭枚举。
//
// 与 UnknownReason 同一套纪律：自由文本无法聚合，事后追溯
// "这条放行当时是怎么来的"需要能按来源分类统计。
type SourceKind string

const (
	// SourceService 表示依据来自 Service 快照。
	SourceService SourceKind = "SERVICE"
	// SourceEndpoints 表示依据来自 Endpoints 快照。
	SourceEndpoints SourceKind = "ENDPOINTS"
	// SourceGateway 表示依据来自 Gateway 或 Ingress 快照。
	SourceGateway SourceKind = "GATEWAY"
	// SourceScrapeTarget 表示依据来自 metrics 抓取关系快照。
	SourceScrapeTarget SourceKind = "SCRAPE_TARGET"
	// SourceAPIServerEndpoint 表示依据来自 API server 端点快照。
	SourceAPIServerEndpoint SourceKind = "APISERVER_ENDPOINT"
	// SourceNodeAgent 表示依据来自节点级 agent 快照。
	SourceNodeAgent SourceKind = "NODE_AGENT"
	// SourceClusterRegistry 表示依据来自集群网段注册信息。
	SourceClusterRegistry SourceKind = "CLUSTER_REGISTRY"
)

// Derivation 指向推导所依据的快照对象。
type Derivation struct {
	// SourceKind 是来源类型。
	SourceKind SourceKind `json:"sourceKind"`
	// Cluster 是来源对象所属集群。
	Cluster string `json:"cluster"`
	// Namespace 是来源对象所属命名空间；集群级对象为空。
	Namespace string `json:"namespace"`
	// Name 是来源对象名。
	Name string `json:"name"`
	// Field 是具体取用的字段，如 spec.selector、healthCheckSources。
	//
	// 精确到字段而非只记对象：同一个 Service 既提供 selector 也提供端口，
	// 只记对象名的追溯回答不了"这个网段是从哪一行来的"。
	Field string `json:"field"`
}

// ErrNoDerivation 表示试图构造没有推导依据的 Baseline 规则。
var ErrNoDerivation = errors.New("baseline rule requires at least one derivation")

// Rule 是一条 Baseline 及其推导依据。
//
// Direction 复用 replay.Direction，不另起一套 INGRESS / EGRESS 枚举：
// replay 不依赖本包，此处无环，而两个同值枚举并存迟早漂移。
type Rule struct {
	// Kind 是 Baseline 类型。
	Kind Kind `json:"kind"`
	// Direction 是规则方向。
	Direction replay.Direction `json:"direction"`
	// Ingress 在 Direction 为 INGRESS 时非空。
	Ingress *networkingv1.NetworkPolicyIngressRule `json:"-"`
	// Egress 在 Direction 为 EGRESS 时非空。
	Egress *networkingv1.NetworkPolicyEgressRule `json:"-"`
	// Derivations 是推导依据，构造时保证非空。
	Derivations []Derivation `json:"derivations"`
}

// NewRule 构造一条 Baseline 规则，拒绝没有推导依据的规则。
func NewRule(
	kind Kind,
	dir replay.Direction,
	ing *networkingv1.NetworkPolicyIngressRule,
	eg *networkingv1.NetworkPolicyEgressRule,
	derivations []Derivation,
) (Rule, error) {
	if len(derivations) == 0 {
		return Rule{}, ErrNoDerivation
	}
	return Rule{
		Kind: kind, Direction: dir, Ingress: ing, Egress: eg,
		Derivations: derivations,
	}, nil
}
