package collectstore

import (
	"testing"

	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
)

// **本集群的节点地址没有 Pod 主体，这是一个结论，不是一次采集缺口。**
//
// 节点按定义就不是 Pod。把它报成 SNAPSHOT_MISSING，运维会照着去查"哪次
// 采集漏了快照"，而根本没有这回事 —— 与 lbIngressIPs、以及同网段里别的机器
// 那两条是同一形状的错（unknownReasonFor 上方的注释把这个形状记了两遍）。
//
// UAT 实测：847 条 SNAPSHOT_MISSING 里 847 条都有一端是本集群节点地址，
// 一条 pod↔pod 都没有；其中 707 条走的是已采到的 kubelet 探针端口 ——
// 那正是 KUBELET_PROBE 基线覆盖的那批流量。
func TestThisClustersNodeAddressIsNotASnapshotGap(t *testing.T) {
	tr := hostNetTraffic(t)
	tr.nodeIPs = map[string]bool{"10.170.48.9": true}
	tr.lbIngressIPs = map[string]bool{}
	a := attributed{
		srcOutcome: identity.OutcomeNotCovered,
		dstOutcome: identity.OutcomeResolved,
	}
	a.conn.Source.IP = "10.170.48.9"
	a.conn.Dest.IP = "172.16.5.7"

	got := tr.unknownReasonFor(a)
	if got == replay.ReasonSnapshotMissing {
		t.Fatal("本集群节点地址被报成 SNAPSHOT_MISSING —— 运维会去查一次不存在的采集缺口")
	}
	if got != replay.ReasonNodeAddress {
		t.Errorf("unknownReasonFor() = %q, want NODE_ADDRESS", got)
	}
}

// **Pod 网段里解不出主体仍然是真的缺口，这一支不许被上面那条带歪。**
func TestAPodAddressWithoutIdentityIsStillASnapshotGap(t *testing.T) {
	tr := hostNetTraffic(t)
	tr.nodeIPs = map[string]bool{"10.170.48.9": true}
	tr.lbIngressIPs = map[string]bool{}
	a := attributed{
		srcOutcome: identity.OutcomeNotCovered,
		dstOutcome: identity.OutcomeResolved,
	}
	a.conn.Source.IP = "172.16.9.9"
	a.conn.Dest.IP = "172.16.5.7"

	if got := tr.unknownReasonFor(a); got != replay.ReasonSnapshotMissing {
		t.Errorf("unknownReasonFor() = %q, want SNAPSHOT_MISSING —— Pod 网段里解不出主体是真的缺口", got)
	}
}

// 节点清单为空时退回原判据：空是"我不知道哪些是节点"，不是"一个节点都没有"。
// 宁可多报缺口也不许少报（同 notThisClustersNode 那条注释）。
func TestAnEmptyNodeListStillReportsAGap(t *testing.T) {
	tr := hostNetTraffic(t)
	tr.nodeIPs = map[string]bool{}
	tr.lbIngressIPs = map[string]bool{}
	a := attributed{srcOutcome: identity.OutcomeNotCovered, dstOutcome: identity.OutcomeResolved}
	a.conn.Source.IP = "10.170.48.9"
	a.conn.Dest.IP = "172.16.5.7"

	if got := tr.unknownReasonFor(a); got != replay.ReasonSnapshotMissing {
		t.Errorf("unknownReasonFor() = %q, want SNAPSHOT_MISSING —— 读不到节点清单时不许少报缺口", got)
	}
}
