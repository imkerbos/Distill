package cluster

// CNI 是集群的网络插件。封闭枚举。
//
// **这是一个事实，不是一个判断。** 平台报告"这个集群跑着 Cilium"，
// 而**不**报告"Cilium 执不执行 ANP" —— 后者会变成一张随版本过时的表，
// 而过时的那天没有任何东西会报错（CLAUDE.md：不得硬编码常量表）。
//
// 它存在的理由：第二策略平面（CNP / ANP / Calico 私有策略）是否真的生效，
// 取决于 CNI。平台把这个事实呈现出来，让读的人自己判断 —— 而平台自己
// 照旧走保守路线：探测到第二平面就降级，不管 CNI 是什么
// （design doc 2026-08-25-existing-policies §2.2）。
type CNI string

const (
	// CNIUnknown 表示认不出来。**不是"没有 CNI"** —— 每个集群都有 CNI，
	// 认不出只说明平台不认识它，或者采集里没有 kube-system 的 Pod。
	CNIUnknown CNI = "UNKNOWN"
	// CNICilium 是 Cilium。
	CNICilium CNI = "CILIUM"
	// CNICalico 是 Calico。
	CNICalico CNI = "CALICO"
)

// cniMarkers 是各 CNI 在 kube-system 里的标志性标签值。
//
// 按**标签**认而不是按 Pod 名前缀：名字带哈希后缀、随部署方式变
// （DaemonSet / Deployment / operator 生成的名字各不相同），而这几个标签
// 是各自项目的官方部署清单里写死的。
//
// 只列平台确实见过的两个。**认不出的一律 UNKNOWN**，不做模糊匹配 ——
// 一个"名字里带 net 就算 CNI"的规则，迟早会把某个业务 workload 认成 CNI。
var cniMarkers = map[string]CNI{
	"cilium":       CNICilium,
	"cilium-agent": CNICilium,
	"calico-node":  CNICalico,
	"calico-typha": CNICalico,
}

// cniLabelKeys 是会承载上面那些标志值的标签键。
var cniLabelKeys = []string{"k8s-app", "app.kubernetes.io/name", "app"}

// CNIPod 是认 CNI 需要的那一点点 Pod 信息。
//
// 收窄成两个字段而不是收整个快照类型：这一步只看 kube-system 里的标签，
// 传进来一个完整的 Pod 会让这个纯函数跟着快照结构一起演进。
type CNIPod struct {
	Namespace string
	Labels    map[string]string
}

// DetectCNI 从 kube-system 的 Pod 里认出集群用的 CNI。
//
// **认不出就答 UNKNOWN，不猜。** 一个猜出来的 CNI 会让下游据此做判断
// （比如"这个 CNI 不执行 ANP，所以那些对象是死的，不必降级"），
// 而猜错的方向是把一个真的在执行的平面当成死的。
//
// **认出多个也答 UNKNOWN。** 一个同时装着两套 CNI 的集群（迁移中、
// 或者装错了）本身就是要被修的东西；挑一个作答等于替运维隐瞒了它。
//
// 只看 kube-system：CNI 按约定装在那里，而一个业务命名空间里叫 cilium 的
// workload 不是 CNI。
func DetectCNI(pods []CNIPod) CNI {
	found := map[CNI]bool{}
	for _, p := range pods {
		if p.Namespace != "kube-system" {
			continue
		}
		for _, key := range cniLabelKeys {
			if cni, ok := cniMarkers[p.Labels[key]]; ok {
				found[cni] = true
			}
		}
	}
	if len(found) != 1 {
		return CNIUnknown
	}
	for cni := range found {
		return cni
	}
	return CNIUnknown
}
