package registry

// EnforcedPlane 是一个**这个集群的 CNI 真的会执行**的策略平面。封闭枚举。
//
// **平台解释任何策略，前提都是 CNI 真的会执行它。** 标准 NetworkPolicy 是
// 所有 CNI 都执行的（Kubernetes 的最低要求），第二平面不是 —— 实测
// （2026-08-26）：原生 Calico v3.30.4 执行 AdminNetworkPolicy，
// 而 Cilium 1.19.5 完全不实现它，整个模块里零处引用。
//
// 因此这一栏是**操作者的事实声明**，不是平台的探测结果：探测只能回答
// "集群里有没有这类对象"，回答不了"它们是不是活的"。两者分开存
// （OtherPlanes 是前者，这一栏是后者）。
//
// **为什么不由平台查表判断**：那张表会随 CNI 版本过时，而过时的那天没有
// 任何东西会报错（CLAUDE.md：不得硬编码常量表）。更糟的是它的用途 ——
// 一旦表过时，方向恰好是把一个真在执行的平面当成死的。
type EnforcedPlane string

const (
	// PlaneAdminNetworkPolicy 是 AdminNetworkPolicy 与 BaselineAdminNetworkPolicy。
	//
	// 两者算一个平面：它们是同一条求值链上的两端（ANP 在标准 NetworkPolicy
	// 之前，BANP 在之后），声明执行其一而不执行另一个没有意义。
	PlaneAdminNetworkPolicy EnforcedPlane = "ADMIN_NETWORK_POLICY"
	// PlaneCiliumNetworkPolicy 是 CiliumNetworkPolicy 与其集群级变体。
	PlaneCiliumNetworkPolicy EnforcedPlane = "CILIUM_NETWORK_POLICY"
	// PlaneCalicoNetworkPolicy 是 Calico 私有的 NetworkPolicy 与 GlobalNetworkPolicy。
	PlaneCalicoNetworkPolicy EnforcedPlane = "CALICO_NETWORK_POLICY"
)

var allEnforcedPlanes = []EnforcedPlane{
	PlaneAdminNetworkPolicy,
	PlaneCiliumNetworkPolicy,
	PlaneCalicoNetworkPolicy,
}

// AllEnforcedPlanes 是枚举的唯一登记处，供校验与界面共用。
func AllEnforcedPlanes() []EnforcedPlane {
	out := make([]EnforcedPlane, len(allEnforcedPlanes))
	copy(out, allEnforcedPlanes)
	return out
}

// Valid 报告该取值是否已登记。
func (p EnforcedPlane) Valid() bool {
	for _, known := range allEnforcedPlanes {
		if p == known {
			return true
		}
	}
	return false
}

// Enforces 报告这个集群是否声明过某个平面**真的在执行**。
//
// **只在操作者明示过时才答 true。** 探测到平面存在不代表 CNI 执行它，
// 而把"存在"当成"执行"会让平台按一套并不生效的语义去算 —— 算出来的 DENY
// 会让它不为那条连接生成放行规则，下发之后真的被拦断。
//
// 未声明时答 false，也就是"照旧不解释这个平面、整片降级"—— 保守方向。
func (c Cluster) Enforces(p EnforcedPlane) bool {
	for _, declared := range c.EnforcedPlanes {
		if declared == p {
			return true
		}
	}
	return false
}
