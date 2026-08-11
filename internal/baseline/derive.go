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
}

// Derive 推导指定 (cluster, namespace) 的全部 Baseline。
//
// DNS 与 control plane 与 namespace 无关，每个 namespace 都需要；
// LB 健康检查、metrics 抓取、节点级 agent 取决于该 namespace 是否
// 真的有对应的暴露面、抓取关系与 agent —— 没有就不生成，由 Missing
// 如实报出，而不是补一条占位规则让齐备性校验通过。
func Derive(a snapshot.Assets, namespace string) Set {
	set := Set{Cluster: a.ClusterID, Namespace: namespace}
	set.Rules = append(set.Rules, deriveDNS(a)...)
	set.Rules = append(set.Rules, deriveControlPlane(a)...)
	set.Rules = append(set.Rules, deriveNodeAgent(a)...)
	set.Rules = append(set.Rules, deriveMetrics(a, namespace)...)
	for _, r := range deriveLBHealth(a) {
		// LB Baseline 按暴露面所在 namespace 归属：一个 namespace 的入口
		// 暴露不该给另一个 namespace 放行健康检查网段。
		for _, d := range r.Derivations {
			if d.SourceKind == SourceGateway && d.Namespace == namespace {
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
func (s Set) Missing() []Kind {
	present := map[Kind]bool{}
	for _, r := range s.Rules {
		present[r.Kind] = true
	}
	out := make([]Kind, 0, len(allKinds))
	for _, k := range allKinds {
		if !present[k] {
			out = append(out, k)
		}
	}
	return out
}
