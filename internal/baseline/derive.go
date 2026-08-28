package baseline

import "github.com/imkerbos/Distill/internal/snapshot"

// Set 是一个 (cluster, namespace) 推导出的全部 Baseline。
type Set struct {
	// Cluster 是所属集群。
	Cluster string `json:"cluster"`
	// Namespace 是所属命名空间。
	Namespace string `json:"namespace"`
	// Rules 是推导出的规则。
	Rules []Rule `json:"rules"`
	// NotApplicable 是这个 namespace 里**没有推导对象**的那几类。
	//
	// 与 Missing() 互斥：一类要么需要而没有（缺失），要么根本不需要
	// （不适用），不会同时是两者。判据见 applicability.go。
	//
	// 报出来而不是静默丢弃：一份空缺失与一次根本没做的校验必须区分得开
	// （同 PolicyPreview.Kinds 的理由）。屏幕上要读得出「batch 的 LB：
	// 看过了，这个 namespace 没有暴露面」。
	NotApplicable []Kind `json:"notApplicable"`
}

// Derive 推导指定 (cluster, namespace) 的全部 Baseline。
//
// 三类是集群级的，对该集群每个 namespace 都成立：DNS、control plane、
// 节点级 agent。节点级 agent 归在这里是因为它采集整个集群的 Pod ——
// 集群里有 agent，每个 namespace 就都要放行它的入向（spec §3）。
//
// 另外两类按 namespace 推导：LB 健康检查取决于该 namespace 有没有暴露面，
// metrics 抓取取决于它是不是抓取目标。没有就不生成，由 Missing 如实报出，
// 而不是补一条占位规则让齐备性校验通过。
//
// 因此 Missing() 对 NODE_AGENT 的判据也是集群级的：没有登记任何 NodeAgent、
// 或 Registry.NodeCIDR 为空时才算缺失，与 namespace 无关。
//
// unassessed 是**依据资源这次没采回来**的那几类。它是必填参数而不是一个
// 事后可选的修正：资产里"这个 namespace 没有暴露对象"与"我们没看过有没有
// 暴露对象"长得一模一样，而把后者判成"不适用"会让一次采集失败变成一次
// 放行 —— 那一类随后从缺失清单里消失，连带绕过 Enforcing 门禁。
// 写成参数，调用方就忘不掉（design doc 2026-08-18-baseline-applicability §3）。
// 全部依据都采回来时传 nil。
func Derive(a snapshot.Assets, namespace string, unassessed []Kind) Set {
	set := Set{Cluster: a.ClusterID, Namespace: namespace}
	set.NotApplicable = notApplicable(a, namespace, unassessed)
	set.Rules = append(set.Rules, deriveDNS(a)...)
	set.Rules = append(set.Rules, deriveControlPlane(a)...)
	set.Rules = append(set.Rules, deriveNodeAgent(a)...)
	set.Rules = append(set.Rules, deriveMetrics(a, namespace)...)
	// EXPOSED_INGRESS 判不出范围时不进 Rules，也不进 NotApplicable（那一支
	// 只在没有暴露对象时打开）：于是 Missing() 自然把它报出来 —— 「有暴露
	// 对象、却推不出放行规则」正是缺口的定义，不需要第三个字段来表达同一件事。
	set.Rules = append(set.Rules, deriveExposedIngress(a, namespace)...)
	for _, r := range deriveLBHealth(a) {
		// LB Baseline 按暴露面所在 namespace 归属：一个 namespace 的入口
		// 暴露不该给另一个 namespace 放行健康检查网段。
		//
		// 按 SourceService 归属而不是 SourceGateway：暴露面可能是 Gateway 的
		// 后端 Service，也可能是一个 LoadBalancer/NodePort Service 直接暴露 ——
		// 后者没有 SourceGateway 溯源。每条 LB 规则都带一条 SourceService，
		// 且它落在暴露面所在的 namespace，是两种暴露共同的归属键。
		for _, d := range r.Derivations {
			if d.SourceKind == SourceService && d.Namespace == namespace {
				set.Rules = append(set.Rules, r)
				break
			}
		}
	}
	return set
}

// Kinds 返回本集合已覆盖的 Baseline 类型，按 AllKinds 的登记顺序。
func (s Set) Kinds() []Kind {
	present := map[Kind]bool{}
	for _, r := range s.Rules {
		present[r.Kind] = true
	}
	out := make([]Kind, 0, len(present))
	for _, k := range allKinds {
		if present[k] {
			out = append(out, k)
		}
	}
	return out
}

// Missing 返回尚未推导出的必备 Baseline 类型，按登记顺序。
//
// 返回缺失清单而非补占位规则：一条编出来的 0.0.0.0/0:53 会让齐备性
// 校验通过，而真正的 DNS 仍然不通。缺 DNS 就是缺 DNS。
//
// **不适用的那几类不在此列。** 一个没有暴露面的 namespace 没有健康检查
// 流量要放行，报它缺 LB_HEALTH_CHECK 是一条误报，而误报会把整份清单的
// 可信度一起拖垮（design doc 2026-08-18-baseline-applicability §1）。
// 有推导对象却推不出规则的，一条都没少报。
func (s Set) Missing() []Kind {
	present := map[Kind]bool{}
	for _, r := range s.Rules {
		present[r.Kind] = true
	}
	for _, k := range s.NotApplicable {
		present[k] = true
	}
	out := make([]Kind, 0, len(allKinds))
	for _, k := range allKinds {
		if !present[k] {
			out = append(out, k)
		}
	}
	return out
}
