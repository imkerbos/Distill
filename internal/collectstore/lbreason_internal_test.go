package collectstore

import (
	"testing"

	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
)

// lbIP 是被测 LoadBalancer 的入口地址，落在登记的 node_cidr（10.170.48.0/24）
// 里 —— 真实事故的形状：入口地址天生在节点网段内，这正是它此前被网段判定
// 误读成 SNAPSHOT_MISSING 的原因。
const lbIP = "10.170.48.18"

// lbTraffic 在 hostNetTraffic 的登记之上，把 lbIP 登记成一个 LB 入口地址。
func lbTraffic(t *testing.T) traffic {
	t.Helper()
	tr := hostNetTraffic(t)
	tr.lbIngressIPs = map[string]bool{lbIP: true}
	return tr
}

// 一端是 LB 入口地址、另一端是集群内一个真实的缺口时，报的必须是缺口。
//
// 旧判断既不看这一端解没解出来，也不分是哪一端，且排在 inClusterGap 之前：
// 于是这条连接被报成 LB_INGRESS_ADDRESS —— 一句"这里本来就没有主体"，而
// 目的端那个 NOT_COVERED 是我们数据里一个真实的洞。运维照着这句话不去查
// 采集，而 SNAPSHOT_MISSING 的统计口径同时少数了一条。
// 这与同一个函数里"集群内的缺口压过公网"那条纪律是同一件事。
func TestInClusterGapBeatsTheLoadBalancerAddressOnTheOtherEnd(t *testing.T) {
	tr := lbTraffic(t)
	a := attributed{
		conn:       conn(lbIP, "172.16.3.9"),
		srcOutcome: identity.OutcomeNoData,
		dstOutcome: identity.OutcomeNotCovered,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonSnapshotMissing {
		t.Errorf("UnknownReason = %q, want SNAPSHOT_MISSING —— 目的端是集群内一个"+
			"真实的缺口，源端是不是 LB 入口不改变这件事", got)
	}
}

// LB 入口地址那一端**解出来了**的时候，这条连接判得出来，不许短路成 UNKNOWN。
//
// MetalLB L2、kube-vip、以及自建集群的常见形态里，LB VIP 就挂在某个节点
// 地址上，而那个节点上跑着 hostNetwork Pod —— 身份解析于是给出 HOST_NETWORK，
// 一个结论。旧判断不看 Outcome，把它一并短路：这条连接不进求值、学不出规则，
// 对端 workload 因此少掉一条入站放行，而候选策略是 default-deny。
// 开发时对着的 GKE 集群 LB VIP 不落在节点上，这一支一次都没走到过。
func TestResolvedLoadBalancerAddressIsStillEvaluated(t *testing.T) {
	tr := lbTraffic(t)
	a := attributed{
		conn:       conn(lbIP, "172.16.3.9"),
		srcOutcome: identity.OutcomeHostNetwork,
		dstOutcome: identity.OutcomeResolved,
	}
	if got := tr.unknownReasonFor(a); got != replay.ReasonNone {
		t.Errorf("UnknownReason = %q, want none —— 两端都解出来了，这条连接判得出来", got)
	}
}

// 修完之后原本那件事仍然成立：解不出主体的 LB 入口地址报 LB_INGRESS_ADDRESS，
// 不报 SNAPSHOT_MISSING。两端各测一次，钉住这一支是按端判的。
func TestUnresolvedLoadBalancerAddressStillReportsItsOwnReason(t *testing.T) {
	for _, c := range []struct {
		name string
		a    attributed
	}{{
		name: "源端是 LB 入口",
		a: attributed{
			conn:       conn(lbIP, "172.16.3.9"),
			srcOutcome: identity.OutcomeNoData,
			dstOutcome: identity.OutcomeResolved,
		},
	}, {
		name: "目的端是 LB 入口",
		a: attributed{
			conn:       conn("172.16.3.9", lbIP),
			srcOutcome: identity.OutcomeResolved,
			dstOutcome: identity.OutcomeNoData,
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			tr := lbTraffic(t)
			if got := tr.unknownReasonFor(c.a); got != replay.ReasonLBIngressAddress {
				t.Errorf("UnknownReason = %q, want LB_INGRESS_ADDRESS", got)
			}
		})
	}
}
