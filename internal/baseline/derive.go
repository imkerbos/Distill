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
