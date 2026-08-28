package collectstore

import (
	"testing"

	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
)

// 登记的 node_cidr 是 10.170.48.0/24；这个集群实际只有一个节点。
const (
	realNodeIP  = "10.170.48.7"
	neighbourIP = "10.170.48.193" // 同子网里另一台机器，不是这个集群的节点
)

func nodeTraffic(t *testing.T) traffic {
	t.Helper()
	tr := hostNetTraffic(t)
	tr.nodeIPs = map[string]bool{realNodeIP: true}
	return tr
}

// 落在登记 node 网段、却不在实际节点清单里的地址不是快照缺口。
//
// 登记的是一个网段，而集群节点通常只占其中一部分——同子网里还有数据库、
// 跳板机、别的集群的节点。按网段判，它们全被算成"集群内解不出主体"，
// 也就是一个并不存在的缺口，运维照着 SNAPSHOT_MISSING 去查"哪次采集漏了
// 快照"，而根本没有这回事。
//
// UAT 实测：node 网段里出现过 29 个地址，15 个是节点、14 个不是，
// 而那 14 个正是 SNAPSHOT_MISSING 的主体。
func TestAnAddressInTheNodeCIDRThatIsNotANodeIsNotAGap(t *testing.T) {
	tr := nodeTraffic(t)
	a := attributed{
		conn:       conn(neighbourIP, "172.16.3.9"),
		srcOutcome: identity.OutcomeNoData,
		dstOutcome: identity.OutcomeResolved,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonExternalNoIdentity {
		t.Errorf("UnknownReason = %q, want EXTERNAL_NO_IDENTITY —— "+
			"它是同子网里的另一台机器，本来就没有 Pod 主体，不是缺口", got)
	}
}

// **真节点仍然要按集群内判。** 这一条守的是上面那条不许扩大化：
// 一个节点地址解不出主体，是我们数据里一个真实的洞。
func TestARealNodeAddressStillCountsAsAGap(t *testing.T) {
	tr := nodeTraffic(t)
	a := attributed{
		conn:       conn(realNodeIP, "172.16.3.9"),
		srcOutcome: identity.OutcomeNoData,
		dstOutcome: identity.OutcomeResolved,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonSnapshotMissing {
		t.Errorf("UnknownReason = %q, want SNAPSHOT_MISSING —— "+
			"它确实是这个集群的节点，解不出主体是真的缺口", got)
	}
}

// **Pod 网段一步都不许动。** 那里解不出主体永远是真的缺口，
// 与节点清单无关。
func TestThePodCIDRIsUntouchedByTheNodeList(t *testing.T) {
	tr := nodeTraffic(t)
	a := attributed{
		conn:       conn("172.16.3.9", "172.16.4.2"),
		srcOutcome: identity.OutcomeNotCovered,
		dstOutcome: identity.OutcomeResolved,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonSnapshotMissing {
		t.Errorf("UnknownReason = %q, want SNAPSHOT_MISSING —— Pod 网段里解不出"+
			"主体是真的缺口，这次改动不该碰到它", got)
	}
}

// 节点清单为空时（读不到、或这一刻还没采到节点）**退回按网段判**，
// 不是把整个 node 网段都当成外部。
//
// 空清单是"我不知道哪些是节点"，而不是"一个节点都没有"。据此把所有节点
// 地址判成外部，等于用一次读取失败换掉一整类真实缺口的可见性。
func TestAnEmptyNodeListFallsBackToTheCIDR(t *testing.T) {
	tr := hostNetTraffic(t) // nodeIPs 为 nil
	a := attributed{
		conn:       conn(neighbourIP, "172.16.3.9"),
		srcOutcome: identity.OutcomeNoData,
		dstOutcome: identity.OutcomeResolved,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonSnapshotMissing {
		t.Errorf("UnknownReason = %q, want SNAPSHOT_MISSING —— 节点清单为空时"+
			"必须退回按网段判，宁可多报缺口也不许少报", got)
	}
}
