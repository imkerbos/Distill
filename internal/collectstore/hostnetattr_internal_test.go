package collectstore

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
)

var attrAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// hostNetInterval 造一个 hostNetwork Pod 在节点 IP 上的开放区间。
func hostNetInterval(ip, name string) identity.Interval {
	return identity.Interval{
		ClusterID: "c1", PodIP: ip, ValidFrom: attrAt.Add(-time.Hour),
		Identity: identity.Identity{
			Namespace: "kube-system", PodName: name, HostNetwork: true,
		},
	}
}

// **hostNetwork 那一端不该在 attribute() 里被翻回 UNKNOWN。**
//
// 已有的 TestHostNetworkEndpointIsNotAnUnknownReason 只测到 unknownReasonFor
// 就停 —— 而它正好停在缺陷前一步。放行之后 attribute() 对两端调 podRefOf，
// 按 (namespace, podName) 查锚点快照；而 identity.Resolve 在多个 hostNetwork
// 区间覆盖同一时刻时返回**零值 Identity**（它自己的注释：「具体是哪一个 Pod
// 既答不出也不重要」）。于是查 podKey{"",""}，落空，报 SNAPSHOT_MISSING
// 「区间与快照对不上」—— 而根本没有什么对不上：那一端就是没有 Pod 主体。
//
// UAT 实测：这一条产出了全部 794 条残余 SNAPSHOT_MISSING。一个节点上跑着
// 十个 hostNetwork DaemonSet，多区间覆盖是常态，零值 Identity 因此也是常态。
func TestAHostNetworkEndpointDoesNotBecomeASnapshotGap(t *testing.T) {
	tr := hostNetTraffic(t)
	tr.at = attrAt
	tr.window = flow.Window{From: attrAt.Add(-time.Minute), To: attrAt.Add(time.Minute)}
	tr.eval = replay.NewEvaluator("c1", nil, nil)
	tr.pods = map[podKey]observedPod{
		{namespace: "g32-game", name: "api-0"}: {
			namespace: "g32-game", name: "api-0", labels: map[string]string{"app": "api"},
		},
	}
	tr.intervals = map[string][]identity.Interval{
		// 同一个节点 IP 上两个 hostNetwork Pod —— 真实集群的常态。
		"10.170.48.99": {
			hostNetInterval("10.170.48.99", "kube-proxy-x"),
			hostNetInterval("10.170.48.99", "filebeat-y"),
		},
		"172.16.3.9": {{
			ClusterID: "c1", PodIP: "172.16.3.9", ValidFrom: attrAt.Add(-time.Hour),
			Identity: identity.Identity{Namespace: "g32-game", PodName: "api-0"},
		}},
	}

	a := tr.attribute(conn("10.170.48.99", "172.16.3.9"))
	if a.srcOutcome != identity.OutcomeHostNetwork {
		t.Fatalf("前提不成立：srcOutcome = %q, want HOST_NETWORK", a.srcOutcome)
	}
	if a.decision.Verdict == replay.VerdictUnknown &&
		a.decision.UnknownReason == replay.ReasonSnapshotMissing {
		t.Fatalf("hostNetwork 端点被报成 SNAPSHOT_MISSING：%q —— "+
			"区间与快照并没有对不上，那一端本来就没有 Pod 主体",
			a.decision.Reason.Detail)
	}
}

// **区间与快照真的对不上时仍然报 SNAPSHOT_MISSING。**
// 上面那条放宽的只有 hostNetwork 一种，不得把真缺口一起放过去。
func TestAResolvedPodMissingFromTheSnapshotIsStillAGap(t *testing.T) {
	tr := hostNetTraffic(t)
	tr.at = attrAt
	tr.window = flow.Window{From: attrAt.Add(-time.Minute), To: attrAt.Add(time.Minute)}
	tr.eval = replay.NewEvaluator("c1", nil, nil)
	tr.pods = map[podKey]observedPod{} // 快照里什么都没有
	tr.intervals = map[string][]identity.Interval{
		"172.16.3.9": {{
			ClusterID: "c1", PodIP: "172.16.3.9", ValidFrom: attrAt.Add(-time.Hour),
			Identity: identity.Identity{Namespace: "g32-game", PodName: "api-0"},
		}},
		"172.16.4.2": {{
			ClusterID: "c1", PodIP: "172.16.4.2", ValidFrom: attrAt.Add(-time.Hour),
			Identity: identity.Identity{Namespace: "g32-game", PodName: "api-1"},
		}},
	}

	a := tr.attribute(conn("172.16.3.9", "172.16.4.2"))
	if a.decision.UnknownReason != replay.ReasonSnapshotMissing {
		t.Errorf("UnknownReason = %q, want SNAPSHOT_MISSING —— 区间说有、快照里没有，是真缺口",
			a.decision.UnknownReason)
	}
}
