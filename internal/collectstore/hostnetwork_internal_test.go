package collectstore

import (
	"net/netip"
	"testing"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
)

// hostNetTraffic 造一个带 fleet 登记的 traffic，供判定原因的用例使用。
func hostNetTraffic(t *testing.T) traffic {
	t.Helper()
	pfx := func(s string) netip.Prefix {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("ParsePrefix %s: %v", s, err)
		}
		return p
	}
	reg := cluster.NewRegistry([]cluster.Cluster{{
		ID:       "c1",
		PodCIDRs: []netip.Prefix{pfx("172.16.0.0/16")},
		// 节点网段登记进来才分得清「节点 IP」与「公网地址」——
		// 少了它，hostNetwork 那一端会被判成 external。
		NodeCIDRs: []netip.Prefix{pfx("10.170.48.0/24")},
	}})
	return traffic{described: described{clusterID: "c1"}, fleet: reg}
}

func conn(src, dst string) flow.Connection {
	return flow.Connection{
		Source: flow.Endpoint{IP: src}, Dest: flow.Endpoint{IP: dst},
		Protocol: "TCP", Port: 8080,
	}
}

// hostNetwork 那一端不该被计进「无法判定」。
//
// 这条来自真集群：一个节点上跑着十个 hostNetwork 的 DaemonSet Pod，全都用
// 节点 IP，身份解析据此判 AMBIGUOUS，下游翻成 IP_AMBIGUOUS —— 实测那个集群
// 26.3% 的 UNKNOWN 里绝大部分是这件事。
//
// 而真相是一个确定的结论：那一端不在 Pod 网络里，NetworkPolicy 选不中它。
// 把确定的事实计进「无法判定」，会让那个比例失去意义 —— 而它正是平台用来
// 说明自己能力边界的数。
func TestHostNetworkEndpointIsNotAnUnknownReason(t *testing.T) {
	tr := hostNetTraffic(t)
	a := attributed{
		conn:       conn("10.170.48.99", "172.16.3.9"),
		srcOutcome: identity.OutcomeHostNetwork,
		dstOutcome: identity.OutcomeResolved,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonNone {
		t.Errorf("UnknownReason = %q, want none —— hostNetwork 是结论，不是弃权", got)
	}
}

// 两端都是 hostNetwork 同样算判得出来。
func TestHostNetworkOnBothEndsIsStillResolved(t *testing.T) {
	tr := hostNetTraffic(t)
	a := attributed{
		conn:       conn("10.170.48.99", "10.170.48.7"),
		srcOutcome: identity.OutcomeHostNetwork,
		dstOutcome: identity.OutcomeHostNetwork,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonNone {
		t.Errorf("UnknownReason = %q, want none", got)
	}
}

// 真歧义照旧报 IP_AMBIGUOUS：放宽的只有 hostNetwork 那一种。
func TestRealAmbiguityStillReportsIPAmbiguous(t *testing.T) {
	tr := hostNetTraffic(t)
	a := attributed{
		conn:       conn("172.16.3.9", "172.16.4.2"),
		srcOutcome: identity.OutcomeAmbiguous,
		dstOutcome: identity.OutcomeResolved,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonIPAmbiguous {
		t.Errorf("UnknownReason = %q, want IP_AMBIGUOUS", got)
	}
}

// 集群内真正的快照缺口照旧报 SNAPSHOT_MISSING。
func TestInClusterGapStillReportsSnapshotMissing(t *testing.T) {
	tr := hostNetTraffic(t)
	a := attributed{
		conn:       conn("172.16.3.9", "172.16.4.2"),
		srcOutcome: identity.OutcomeNotCovered,
		dstOutcome: identity.OutcomeResolved,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonSnapshotMissing {
		t.Errorf("UnknownReason = %q, want SNAPSHOT_MISSING", got)
	}
}

// hostNetwork 那一端必须被标成「不受管控」。
//
// 多个 Pod 共用节点 IP 时 Identity 是零值，所以不能只看 src.HostNetwork ——
// 那时它是 false，而事实恰恰相反。一条通往特权组件的连接渲染成普通放行，
// 与「策略确实允许了它」无法区分，而两者的处置完全不同。
func TestHostNetworkEndpointIsMarkedUnmanaged(t *testing.T) {
	tr := hostNetTraffic(t)
	a := attributed{
		conn:       conn("10.170.48.99", "172.16.3.9"),
		srcOutcome: identity.OutcomeHostNetwork,
		dstOutcome: identity.OutcomeResolved,
		// Identity 刻意留零值：多个 hostNetwork Pod 共用节点 IP 时就是这样。
	}
	if !tr.unmanaged(a) {
		t.Error("hostNetwork 那一端没有被标成不受管控")
	}
}
