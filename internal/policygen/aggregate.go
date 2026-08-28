package policygen

import (
	"fmt"
	"strings"

	"github.com/imkerbos/Distill/internal/replay"
)

// workloadLabelKeys 是 workload 归属所用的标签键，按优先级排列。
//
// 固定用一组约定标签键而非按 Pod 名截断前缀：真实集群里 Pod 名后缀是
// ReplicaSet 哈希，截断规则会把不同 workload 归成同一个，而且不报错 ——
// 一个能查出结果、看起来正确、实际错误的归并，比一个明确的 UNKNOWN
// 危险得多。
//
// 真实集群不会都用 app：kind 集群上 coredns / kube-proxy 用 k8s-app，
// 控制面组件用 component。只认 app 会让这些确实存在的 workload 从
// 名册里静默消失。顺序即优先级，不接受配置——这条归属规则关系到
// selector 语义，配置化会让同一个平台对同一个 Pod 在不同时间给出
// 不同的 podSelector。
//
// 改动这份顺序——包括调整已有条目的先后——是一次会改变规则指纹的
// 变更：Helm chart 常见同时打 app.kubernetes.io/name 与 app 两个标签且
// 取值不同，某个 workload 命中的键一旦因为改顺序而改变，podSelector
// 的键和值都会变，FingerprintOf 随之改变，该 workload 下所有人工确认
// （override.go 的 Override.Fingerprint）会静默失效——不报错，只是
// 从此再也匹配不上任何规则，页面上表现为"确认过的规则又变回待确认"。
// 目前平台没有已上线用户，这条列表还没被任何真实确认依赖，因此改动
// 安全；上线后再改，必须当成一次破坏性变更来对待，而不是当成一次
// 内部实现调整。
var workloadLabelKeys = []string{
	"app.kubernetes.io/name", // Kubernetes 官方推荐标签，优先级最高
	"app",                    // 事实标准，历史遗留集群的主流写法
	"k8s-app",                // 集群内置组件（coredns、kube-proxy 等）
	"component",              // 控制面组件（kube-apiserver、etcd 等）
}

// labelKeyRank 是 workloadLabelKeys 的优先级索引，值越小越优先。
var labelKeyRank = func() map[string]int {
	out := make(map[string]int, len(workloadLabelKeys))
	for i, k := range workloadLabelKeys {
		out[k] = i
	}
	return out
}()

// WorkloadOf 按优先级从 Pod 标签里选出 workload 归属键与取值。
//
// **导出它是为了让"主体是谁"只有一个定义。** 对账（internal/reconcile 的
// 输入）必须按候选策略的同一个主体聚合：按 A 聚合、按 B 拦门禁，两者对不上时
// 一个分歧率高的 workload 照样能把它的推荐推出去（design doc 2026-08-25 §3.4）。
// 重抄一份判据必然漂移，而漂移的那天没有任何症状。
func WorkloadOf(labels map[string]string) (key, value string, ok bool) {
	return resolveWorkloadLabel(labels)
}

// resolveWorkloadLabel 按优先级从 Pod 标签里选出 workload 归属键与取值。
//
// 返回具体命中的键而非固定假设 app：podSelector 必须用实际命中的键
// 构造，否则一条用 k8s-app 归属出来的 coredns 策略会被拼成
// {app: kube-dns} —— 集群里没有任何 Pod 会命中这个 selector，是一条
// 看起来存在、实际选不中任何对象的幽灵策略。
func resolveWorkloadLabel(labels map[string]string) (key, value string, ok bool) {
	for _, k := range workloadLabelKeys {
		if v := labels[k]; v != "" {
			return k, v, true
		}
	}
	return "", "", false
}

// nsWorkload 是候选策略的身份：(namespace, workload 取值)。
type nsWorkload struct {
	namespace string
	workload  string
}

// keyClaim 是某个 (namespace, workload) 上一次归属键的主张。
type keyClaim struct {
	key string
	// fromRoster 表示这次主张来自 Pod 名册（Input.Pods），而不是流量里
	// 出现过的 Pod 引用。
	fromRoster bool
}

// resolveWinningKeys 为每个 (namespace, workload) 选出唯一的归属标签键。
//
// 同一个 namespace 里两个 Pod 可以带同一个 workload 取值却挂在不同的
// 标签键上 —— Helm chart 改标签的滚动更新期间就是这个形态：旧 ReplicaSet
// 是 app: foo，新的是 app.kubernetes.io/name: foo。若两边各自建一条候选
// 策略，(namespace, workload) 就不再唯一，而这个二元组是覆盖机制的定位
// 键：Apply 只会命中第一条、EnsureRuleExists 却两条都认，于是一条通过了
// 写入校验的覆盖在展示时永远显示「已失效」；EnabledPolicies 还会吐出两条
// 同名 NetworkPolicy，排序比较器在这里打平，输出顺序退化成 map 遍历顺序
// —— 而整个指纹机制挂在那条确定性上。
//
// 因此这里先定一个赢家，输家由调用方按缺口报出去（ExclusionLabelKeyConflict
// / ReasonLabelKeyConflict），而不是让两条策略并存。
//
// 名册优先于流量：赢家决定 podSelector 的键，而 podSelector 要选中的是
// 当前活着的 Pod。让一条引用了已删除 Pod 的历史流量把赢家改掉，生成的
// 就是一条集群里没有任何 Pod 会命中的幽灵策略。
func resolveWinningKeys(in Input) map[nsWorkload]string {
	claims := map[nsWorkload]keyClaim{}
	consider := func(p replay.PodRef, fromRoster bool) {
		if p.ClusterID != in.ClusterID || replay.IsUnmanaged(p) {
			return
		}
		key, wl, ok := resolveWorkloadLabel(p.Labels)
		if !ok {
			return
		}
		id := nsWorkload{namespace: p.Namespace, workload: wl}
		cur, seen := claims[id]
		if !seen || betterClaim(keyClaim{key: key, fromRoster: fromRoster}, cur) {
			claims[id] = keyClaim{key: key, fromRoster: fromRoster}
		}
	}
	for _, p := range in.Pods {
		consider(p, true)
	}
	for _, o := range in.Observations {
		if o.Flow.Source.Pod != nil {
			consider(*o.Flow.Source.Pod, false)
		}
		if o.Flow.Dest.Pod != nil {
			consider(*o.Flow.Dest.Pod, false)
		}
	}
	out := make(map[nsWorkload]string, len(claims))
	for id, c := range claims {
		out[id] = c.key
	}
	return out
}

// betterClaim 报告 a 是否该取代 b。
//
// 判据只有名册来源与固定优先级两条，都与遍历顺序无关：赢家不能依赖
// map 的迭代顺序，否则同一份输入两次生成会得到不同的 podSelector 键。
func betterClaim(a, b keyClaim) bool {
	if a.fromRoster != b.fromRoster {
		return a.fromRoster
	}
	return labelKeyRank[a.key] < labelKeyRank[b.key]
}

// aggKey 是规则的聚合键。
//
// 与 spec §8.3 的 Flow Identity 同构：对账时 join 键与生成键必须是同一个，
// 否则预测清单对不上实际流量。
type aggKey struct {
	Cluster       string
	Subject       string // 主体 workload 取值，即 podSelector 的标签值
	SubjectKey    string // 主体 workload 命中的标签键，见 workloadLabelKeys
	SubjectNS     string
	PeerNamespace string
	PeerWorkload  string
	PeerKey       string // 对端 workload 命中的标签键；PeerCIDR 非空时无意义
	PeerCIDR      string // 对端不可用 selector 表达时使用
	Direction     replay.Direction
	Protocol      replay.Protocol
	Port          int32
	Evidence      EvidenceClass
}

// keyed 是一条聚合项及其来源流量。
type keyed struct {
	key    aggKey
	flowID string
}

// classify 把一条观测流量拆成对本集群有意义的聚合项。
//
// 一条集群内流量产出两项（源侧 egress + 目的侧 ingress）：少一侧就会
// 生成单向策略，源放行了、目的没放行，上线即断。
//
// 某一侧不可表达不连累另一侧：源端没有 app 标签时，目的端的 ingress
// 规则仍然成立，把它一起丢掉会让候选策略凭空少一条必要放行。
//
// winners 是每个 (namespace, workload) 唯一的归属标签键，见
// resolveWinningKeys：主体侧必须与候选策略的 podSelector 用同一个键，
// 否则学到的规则会挂到一条选不中这个 Pod 的策略上。
func classify(
	o Observation, clusterID string, winners map[nsWorkload]string,
) ([]keyed, []UngeneratableItem) {
	// 整条流量级别的排除先做：这两类与方向无关，逐侧判会重复报两次。
	//
	// 两类各自的含义，**顺序不能反**（见下）：判不出主体是"我不知道这个
	// 地址是谁"，身份不可信是"我知道是谁，但 mesh / CCNP 之后这个身份不
	// 代表真实主体"。后者学出的规则会挂到错的主体上——那不是"证据不够"，
	// 是"证据指向错的对象"。
	//
	// 窗口证明不了完整是第三件事：身份是准的，只是可能没看全，那类走
	// EvidenceIncompleteWindow 生成、默认不启用
	// （design doc 2026-08-18-learn-from-incomplete-evidence §2）。
	// **判不出来的先答判不出来。** IdentityTrusted 只在求值真的跑过时才有
	// 意义：解不开主体的那些连接在 attribute() 里提前返回，那个字段停在零值
	// false，于是先判它等于把每一条"我不知道这个地址是谁"都报成
	// "mesh 或 CCNP 干扰"——一句关于第二策略平面的话，而事实是身份没解开。
	//
	// 代价是 ReasonIdentityUnknown 整个取值不可达，运维照着满屏
	// DEGRADED_EVIDENCE 去查 mesh，而真正要查的 SNAPSHOT_MISSING /
	// EXTERNAL_NO_IDENTITY / LB_INGRESS_ADDRESS 一条都不显示。封闭枚举里
	// 一个永远产不出的取值，比没有这个取值更糟（CLAUDE.md §3）。
	if o.Decision.Verdict == replay.VerdictUnknown {
		return nil, []UngeneratableItem{{
			FlowID: o.FlowID, Reason: ReasonIdentityUnknown,
			Detail: string(o.Decision.UnknownReason),
		}}
	}
	// 到这里两端都解开了、求值跑过了，IdentityTrusted 才是一句有依据的话。
	if !o.IdentityTrusted {
		return nil, []UngeneratableItem{{
			FlowID: o.FlowID, Reason: ReasonDegradedEvidence,
			Detail: "mesh 或 CCNP 干扰，结论不得作为策略推荐依据",
		}}
	}

	evidence := evidenceFor(o)
	var items []keyed
	var bad []UngeneratableItem

	// 源侧：主体是源 Pod，方向为 egress。
	if sub, ok, item := subjectOf(o, o.Flow.Source, clusterID, winners); ok {
		peerNS, peerWL, peerKey, peerCIDR, expressible, peerItem := peerOf(o, o.Flow.Dest, clusterID)
		if expressible {
			items = append(items, keyed{key: aggKey{
				Cluster: clusterID, Subject: sub.workload, SubjectKey: sub.labelKey, SubjectNS: sub.namespace,
				PeerNamespace: peerNS, PeerWorkload: peerWL, PeerKey: peerKey, PeerCIDR: peerCIDR,
				Direction: replay.DirectionEgress,
				Protocol:  o.Flow.Protocol, Port: o.Flow.Port, Evidence: evidence,
			}, flowID: o.FlowID})
		} else if peerItem != nil {
			bad = append(bad, *peerItem)
		}
	} else if item != nil {
		bad = append(bad, *item)
	}

	// 目的侧：主体是目的 Pod，方向为 ingress。
	if sub, ok, item := subjectOf(o, o.Flow.Dest, clusterID, winners); ok {
		peerNS, peerWL, peerKey, peerCIDR, expressible, peerItem := peerOf(o, o.Flow.Source, clusterID)
		if expressible {
			items = append(items, keyed{key: aggKey{
				Cluster: clusterID, Subject: sub.workload, SubjectKey: sub.labelKey, SubjectNS: sub.namespace,
				PeerNamespace: peerNS, PeerWorkload: peerWL, PeerKey: peerKey, PeerCIDR: peerCIDR,
				Direction: replay.DirectionIngress,
				Protocol:  o.Flow.Protocol, Port: o.Flow.Port, Evidence: evidence,
			}, flowID: o.FlowID})
		} else if peerItem != nil {
			bad = append(bad, *peerItem)
		}
	} else if item != nil {
		bad = append(bad, *item)
	}

	return items, bad
}

// subject 是规则主体的标识。
type subject struct {
	namespace string
	workload  string
	labelKey  string // 命中的标签键，podSelector 据此构造，见 workloadLabelKeys
}

// subjectOf 判断该端点能否作为本集群策略的主体。
//
// 第二个返回值为 false 且第三个为 nil 表示"该端点本就不属于本集群"，
// 不是缺陷，无需报告；第三个非 nil 才是真正表达不了的情况。
func subjectOf(
	o Observation, ep replay.Endpoint, clusterID string, winners map[nsWorkload]string,
) (subject, bool, *UngeneratableItem) {
	if ep.Pod == nil || ep.Pod.ClusterID != clusterID {
		return subject{}, false, nil
	}
	if replay.IsUnmanaged(*ep.Pod) {
		return subject{}, false, &UngeneratableItem{
			FlowID: o.FlowID, Reason: ReasonUnmanagedEndpoint,
			Detail: fmt.Sprintf("%s/%s 使用 hostNetwork，NetworkPolicy 管不到",
				ep.Pod.Namespace, ep.Pod.Name),
		}
	}
	key, wl, ok := resolveWorkloadLabel(ep.Pod.Labels)
	if !ok {
		return subject{}, false, &UngeneratableItem{
			FlowID: o.FlowID, Reason: ReasonNoWorkloadLabel,
			Detail: fmt.Sprintf("%s/%s 没有可识别的 workload 标签（%s），podSelector 无法表达",
				ep.Pod.Namespace, ep.Pod.Name, strings.Join(workloadLabelKeys, "/")),
		}
	}
	// 归属键与该 (namespace, workload) 的赢家不一致：这个 Pod 没有属于
	// 自己的候选策略（见 resolveWinningKeys），把它的流量学进赢家那条
	// 策略等于用一个选不中它的 podSelector 放行它的流量 —— 规则会写进
	// 集群、看起来覆盖了这条连接，实际一条都放不通，而人是照着这份清单
	// 判断"上线会不会断"的。
	if win, ok := winners[nsWorkload{namespace: ep.Pod.Namespace, workload: wl}]; ok && win != key {
		return subject{}, false, &UngeneratableItem{
			FlowID: o.FlowID, Reason: ReasonLabelKeyConflict,
			Detail: fmt.Sprintf(
				"%s/%s 用标签键 %s 归属到 workload %q，而该 workload 已由优先级更高的 %s 归属；"+
					"一个 (namespace, workload) 只能有一条候选策略，该 Pod 不在它的 podSelector 范围内",
				ep.Pod.Namespace, ep.Pod.Name, key, wl, win),
		}
	}
	return subject{namespace: ep.Pod.Namespace, workload: wl, labelKey: key}, true, nil
}

// peerOf 把对端表达成 selector 或 ipBlock。
//
// 第五个返回值为 false 表示对端无法表达，该方向不生成规则；第六个返回值
// 非 nil 时是这次表达失败该报的缺口。第三个返回值是对端命中的标签键，
// selector 对端才有意义，ipBlock 对端恒为空。
//
// 对端不做 resolveWinningKeys 那套唯一化：唯一性是候选策略身份的要求
// （一个 (namespace, workload) 一条策略），而对端只是一段 selector 表达式
// —— {k8s-app: foo} 恰好就选中那个 Pod，把它改写成赢家的键反而选不中。
// 两个只差标签键的对端因此是两条不同的规则，FingerprintOf 取规则体而非
// 展示串，两者的指纹也不同（见 describe.go）。
func peerOf(o Observation, ep replay.Endpoint, clusterID string) (
	ns, workload, labelKey, cidr string, ok bool, unexpressible *UngeneratableItem,
) {
	// 本集群内、有身份：只能用 selector，不能退到 IP。
	if ep.Pod != nil && ep.Pod.ClusterID == clusterID {
		// hostNetwork 判断先于标签判断：hostNetwork Pod 用宿主机网络
		// namespace，流量以节点 IP 出现，podSelector 天生选不中它 ——
		// 即便它带 workload 标签，生成的规则也是一条谁都匹配不到的幽灵
		// 规则，却会被分类成 TRUSTED_ALLOW、被 Task 6 标 Enabled=true，
		// 看着正常实则空转。与 Task 4 node-agent Baseline 必须用节点网段
		// 而非 podSelector 是同一个约束，只是这里是对端侧。
		if replay.IsUnmanaged(*ep.Pod) {
			return "", "", "", "", false, &UngeneratableItem{
				FlowID: o.FlowID, Reason: ReasonUnmanagedEndpoint,
				Detail: fmt.Sprintf("对端 %s/%s 使用 hostNetwork，不受 NetworkPolicy 管控",
					ep.Pod.Namespace, ep.Pod.Name),
			}
		}
		if key, wl, hit := resolveWorkloadLabel(ep.Pod.Labels); hit {
			return ep.Pod.Namespace, wl, key, "", true, nil
		}
		// 集群内对端已知身份、受管控、但没有可识别的 workload 标签：不能
		// 退化成 /32 ipBlock —— Pod 重建后 IP 会被别的 workload 复用，
		// 那时这条规则会静默放行错的对象。它命中的证据类型多半是
		// TRUSTED_ALLOW，会被 Task 6 标 Enabled=true，直接进入默认推荐
		// 策略集，是本平台要防的那种"看起来正确、实际错误"的规则。宁可
		// 报 NO_WORKLOAD_LABEL 缺口。
		return "", "", "", "", false, &UngeneratableItem{
			FlowID: o.FlowID, Reason: ReasonNoWorkloadLabel,
			Detail: fmt.Sprintf("对端 %s/%s 没有可识别的 workload 标签（%s），podSelector 无法表达",
				ep.Pod.Namespace, ep.Pod.Name, strings.Join(workloadLabelKeys, "/")),
		}
	}
	// 集群外（互联网、其他集群）只能用 ipBlock。/32 而非对端网段：平台
	// 不知道对端网段的边界，猜一个更宽的会静默放开一片地址。这些地址
	// 不受本平台 Pod 生命周期管理，且对应证据类型（INTERNET_EGRESS /
	// CROSS_CLUSTER）在 Task 6 里默认 Enabled=false，不会因 IP 复用而
	// 静默放行，因此 /32 在这里是安全的。
	if ep.IP == "" {
		return "", "", "", "", false, nil
	}
	return "", "", "", ep.IP + "/32", true, nil
}

// evidenceFor 判定一条流量的证据类型。
//
// 顺序即优先级：跨集群与出公网先于 ALLOW/DENY 判定，因为它们描述的是
// "对端只能用 ipBlock 表达"这一结构性事实，而不是当前策略的结论。
func evidenceFor(o Observation) EvidenceClass {
	// 窗口证明不了完整时，这一条盖过其余分类：操作者要先知道"这条规则的证据
	// 可能不全"，再谈它是出公网还是跨集群。分类只有一个字段，而这一条是
	// 决定它启不启用的那个（design doc §4）。
	if o.Decision.Confidence == replay.ConfidenceDegraded {
		return EvidenceIncompleteWindow
	}
	if o.Decision.CrossCluster {
		return EvidenceCrossCluster
	}
	if isExternal(o.Flow.Dest) || isExternal(o.Flow.Source) {
		return EvidenceInternetEgress
	}
	if o.Decision.Verdict == replay.VerdictDeny {
		return EvidenceTrustedDeny
	}
	return EvidenceTrustedAllow
}

// isExternal 报告端点是否为集群外地址：既无 Pod 身份，也不属于任何集群。
func isExternal(ep replay.Endpoint) bool {
	return ep.Pod == nil && ep.ClusterID == ""
}
