package policygen

import (
	"fmt"

	"github.com/imkerbos/Distill/internal/replay"
)

// workloadLabel 是 workload 归属所用的标签键。
//
// 固定用 app 而非按 Pod 名截断前缀：真实集群里 Pod 名后缀是 ReplicaSet
// 哈希，截断规则会把不同 workload 归成同一个，而且不报错 —— 一个能查出
// 结果、看起来正确、实际错误的归并，比一个明确的 UNKNOWN 危险得多。
const workloadLabel = "app"

// aggKey 是规则的聚合键。
//
// 与 spec §8.3 的 Flow Identity 同构：对账时 join 键与生成键必须是同一个，
// 否则预测清单对不上实际流量。
type aggKey struct {
	Cluster       string
	Subject       string // 主体 workload，即 podSelector 的 app 值
	SubjectNS     string
	PeerNamespace string
	PeerWorkload  string
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
func classify(o Observation, clusterID string) ([]keyed, []UngeneratableItem) {
	// 整条流量级别的排除先做：这两类与方向无关，逐侧判会重复报两次。
	if o.Decision.Confidence == replay.ConfidenceDegraded {
		return nil, []UngeneratableItem{{
			FlowID: o.FlowID, Reason: ReasonDegradedEvidence,
			Detail: "mesh 或 CCNP 干扰，结论不得作为策略推荐依据",
		}}
	}
	if o.Decision.Verdict == replay.VerdictUnknown {
		return nil, []UngeneratableItem{{
			FlowID: o.FlowID, Reason: ReasonIdentityUnknown,
			Detail: string(o.Decision.UnknownReason),
		}}
	}

	evidence := evidenceFor(o)
	var items []keyed
	var bad []UngeneratableItem

	// 源侧：主体是源 Pod，方向为 egress。
	if sub, ok, item := subjectOf(o, o.Flow.Source, clusterID); ok {
		peerNS, peerWL, peerCIDR, expressible, peerItem := peerOf(o, o.Flow.Dest, clusterID)
		if expressible {
			items = append(items, keyed{key: aggKey{
				Cluster: clusterID, Subject: sub.workload, SubjectNS: sub.namespace,
				PeerNamespace: peerNS, PeerWorkload: peerWL, PeerCIDR: peerCIDR,
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
	if sub, ok, item := subjectOf(o, o.Flow.Dest, clusterID); ok {
		peerNS, peerWL, peerCIDR, expressible, peerItem := peerOf(o, o.Flow.Source, clusterID)
		if expressible {
			items = append(items, keyed{key: aggKey{
				Cluster: clusterID, Subject: sub.workload, SubjectNS: sub.namespace,
				PeerNamespace: peerNS, PeerWorkload: peerWL, PeerCIDR: peerCIDR,
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
}

// subjectOf 判断该端点能否作为本集群策略的主体。
//
// 第二个返回值为 false 且第三个为 nil 表示"该端点本就不属于本集群"，
// 不是缺陷，无需报告；第三个非 nil 才是真正表达不了的情况。
func subjectOf(o Observation, ep replay.Endpoint, clusterID string) (subject, bool, *UngeneratableItem) {
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
	wl := ep.Pod.Labels[workloadLabel]
	if wl == "" {
		return subject{}, false, &UngeneratableItem{
			FlowID: o.FlowID, Reason: ReasonNoWorkloadLabel,
			Detail: fmt.Sprintf("%s/%s 没有 %s 标签，podSelector 无法表达",
				ep.Pod.Namespace, ep.Pod.Name, workloadLabel),
		}
	}
	return subject{namespace: ep.Pod.Namespace, workload: wl}, true, nil
}

// peerOf 把对端表达成 selector 或 ipBlock。
//
// 第四个返回值为 false 表示对端无法表达，该方向不生成规则；第五个返回值
// 非 nil 时是这次表达失败该报的缺口。
func peerOf(o Observation, ep replay.Endpoint, clusterID string) (
	ns, workload, cidr string, ok bool, unexpressible *UngeneratableItem,
) {
	// 本集群内、有身份：只能用 selector，不能退到 IP。
	if ep.Pod != nil && ep.Pod.ClusterID == clusterID {
		// hostNetwork 判断先于标签判断：hostNetwork Pod 用宿主机网络
		// namespace，流量以节点 IP 出现，podSelector 天生选不中它 ——
		// 即便它带 app 标签，生成的规则也是一条谁都匹配不到的幽灵规则，
		// 却会被分类成 TRUSTED_ALLOW、被 Task 6 标 Enabled=true，看着
		// 正常实则空转。与 Task 4 node-agent Baseline 必须用节点网段而
		// 非 podSelector 是同一个约束，只是这里是对端侧。
		if replay.IsUnmanaged(*ep.Pod) {
			return "", "", "", false, &UngeneratableItem{
				FlowID: o.FlowID, Reason: ReasonUnmanagedEndpoint,
				Detail: fmt.Sprintf("对端 %s/%s 使用 hostNetwork，不受 NetworkPolicy 管控",
					ep.Pod.Namespace, ep.Pod.Name),
			}
		}
		if wl := ep.Pod.Labels[workloadLabel]; wl != "" {
			return ep.Pod.Namespace, wl, "", true, nil
		}
		// 集群内对端已知身份、受管控、但没有 app 标签：不能退化成 /32
		// ipBlock —— Pod 重建后 IP 会被别的 workload 复用，那时这条规则
		// 会静默放行错的对象。它命中的证据类型多半是 TRUSTED_ALLOW，会
		// 被 Task 6 标 Enabled=true，直接进入默认推荐策略集，是本平台
		// 要防的那种"看起来正确、实际错误"的规则。宁可报 NO_WORKLOAD_LABEL
		// 缺口。
		return "", "", "", false, &UngeneratableItem{
			FlowID: o.FlowID, Reason: ReasonNoWorkloadLabel,
			Detail: fmt.Sprintf("对端 %s/%s 没有 %s 标签，podSelector 无法表达",
				ep.Pod.Namespace, ep.Pod.Name, workloadLabel),
		}
	}
	// 集群外（互联网、其他集群）只能用 ipBlock。/32 而非对端网段：平台
	// 不知道对端网段的边界，猜一个更宽的会静默放开一片地址。这些地址
	// 不受本平台 Pod 生命周期管理，且对应证据类型（INTERNET_EGRESS /
	// CROSS_CLUSTER）在 Task 6 里默认 Enabled=false，不会因 IP 复用而
	// 静默放行，因此 /32 在这里是安全的。
	if ep.IP == "" {
		return "", "", "", false, nil
	}
	return "", "", ep.IP + "/32", true, nil
}

// evidenceFor 判定一条流量的证据类型。
//
// 顺序即优先级：跨集群与出公网先于 ALLOW/DENY 判定，因为它们描述的是
// "对端只能用 ipBlock 表达"这一结构性事实，而不是当前策略的结论。
func evidenceFor(o Observation) EvidenceClass {
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
