package baseline

import "github.com/imkerbos/Distill/internal/snapshot"

// notApplicable 返回在这个 (cluster, namespace) 上**没有推导对象**的那几类，
// 按登记顺序。
//
// 它把 Missing() 今天压着的两件事分开：「需要但推不出来」与「根本不需要」。
// 前者是要修的缺口，后者报出来就是误报 —— 而一条永远在喊、而且喊错的告警
// 会让整类告警被整体忽略，于是真正缺 DNS 的那次也一起被忽略了
// （design doc 2026-08-18-baseline-applicability §1，与 classifyPodIP 的
// 注释同一个教训）。
//
// **这个推断只在这一类可评估时成立。** 依据资源那次采集没拿回来的类别由
// unassessed 传进来，本函数一个都不判 —— 空的资产列表在那种形态下的含义是
// "我们没看过"，而不是"集群里就是没有"。把它读成"不适用"就是把一次采集
// 失败变成一次放行，方向反了（安全规范 §49）。它们照旧留在缺失清单里，
// 由 NotAssessedBaselines 另行标注。
//
// 三类恒适用，一条都不判：DNS 与 control plane 对每个 Pod 都成立；
// NODE_AGENT 的"不适用"是一次写审计的人工声明（node-agent spec §3），
// 平台看不见 agent 连不连工作负载，因此不得在这里推断。
func notApplicable(a snapshot.Assets, namespace string, unassessed []Kind) []Kind {
	blind := make(map[Kind]bool, len(unassessed))
	for _, k := range unassessed {
		blind[k] = true
	}
	var out []Kind
	if !blind[KindLBHealth] && !exposed(a, namespace) {
		out = append(out, KindLBHealth)
	}
	// EXPOSED_INGRESS 复用同一个 exposed() 判据：没有 Gateway/Ingress、
	// 也没有 LoadBalancer/NodePort Service 的 namespace 根本没有对外入口，
	// 判成缺失会给每个内部 namespace 挂一条永远补不上的缺口。
	if !blind[KindExposedIngress] && !exposed(a, namespace) {
		out = append(out, KindExposedIngress)
	}
	if !blind[KindMetrics] && !scrapeDeclared(a, namespace) {
		out = append(out, KindMetrics)
	}
	// 按 allKinds 的登记顺序返回，与 Missing()/Kinds() 一致。
	ordered := make([]Kind, 0, len(out))
	for _, k := range allKinds {
		for _, got := range out {
			if got == k {
				ordered = append(ordered, k)
				break
			}
		}
	}
	return ordered
}

// exposed 报告这个 namespace 有没有会被健康检查打到的入口暴露对象。
//
// **不只看 Gateway。** snapshot 的入口对象本轮只含 Kind=Ingress
// （Observation.Gateways 的注释），只看它会漏掉 type=LoadBalancer 的
// Service —— 那种 namespace 一样有健康检查流量，一样会被 default-deny
// 打断，而症状是入口中断。
//
// NodePort 一并算上：它常作为外部 LB 的后端，同样被健康检查打。把它算成
// 适用的代价是多挡一次，算成不适用的代价是一次入口中断。
func exposed(a snapshot.Assets, namespace string) bool {
	for _, gw := range a.Gateways {
		if gw.Namespace == namespace {
			return true
		}
	}
	for _, svc := range a.Services {
		if svc.Namespace != namespace {
			continue
		}
		if svc.Type == serviceTypeLoadBalancer || svc.Type == serviceTypeNodePort {
			return true
		}
	}
	return false
}

// serviceTypeLoadBalancer 与 serviceTypeNodePort 是 exposed 认的两种暴露型别。
//
// 写成本地常量而不是引 corev1：本包是纯包，而 snapshot.Service.Type 本来
// 就是字符串 —— 为两个字面量把 k8s.io/api 拉进适用性判定不划算。
const (
	serviceTypeLoadBalancer = "LoadBalancer"
	serviceTypeNodePort     = "NodePort"
)

// scrapeDeclared 报告这个 namespace 里有没有 Pod 声明自己要被抓。
//
// **取声明，不取 ScrapeTargets。** 后者是「登记的抓取端 × 观测到的被抓端」
// 拼出来的，一个还没登记任何抓取端的集群它是空的 —— 拿它当判据，每个
// namespace 都会显得不适用，而下发之后真正的 Prometheus 会被挡
// （design doc §4.2）。
func scrapeDeclared(a snapshot.Assets, namespace string) bool {
	for _, d := range a.ScrapeDeclarations {
		if d.Namespace == namespace {
			return true
		}
	}
	return false
}
