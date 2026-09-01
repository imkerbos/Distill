package predict

import (
	"testing"

	"github.com/imkerbos/Distill/internal/replay"
)

// **两侧都判不出时，以归属层的原因为准。**
//
// 求值引擎只看得见 `endpoint.Pod == nil`，它不知道为什么解不开身份，
// 只能给一个占位（evaluator.go 那一处固定返回 SNAPSHOT_MISSING）。归属层
// 知道：那一端是本集群节点、是 LB 入口地址、是集群外地址、还是 IP 复用
// 分不开（decide.go 的 unknownReasonFor 四路分类）。
//
// 取占位值的后果是把四类压成一类，而且压成唯一一个会把人引去查采集的那个。
// UAT 实测：2259 条 UNKNOWN 里 1412 条（62.5%）被这样标错 ——
// 771 条 EXTERNAL_NO_IDENTITY、375 条 LB_INGRESS_ADDRESS、266 条 IP_AMBIGUOUS
// 全部显示成 SNAPSHOT_MISSING。而 LB_INGRESS_ADDRESS 的枚举注释写着
// 「根本没有什么该采而没采的东西」。
func TestTheAttributionReasonWinsWhenBothSidesAreUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		cur  replay.UnknownReason
	}{
		{"LB 入口地址", replay.ReasonLBIngressAddress},
		{"集群外地址", replay.ReasonExternalNoIdentity},
		{"IP 复用分不开", replay.ReasonIPAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := replay.Decision{Verdict: replay.VerdictUnknown, UnknownReason: tc.cur}
			pred := replay.Decision{
				Verdict: replay.VerdictUnknown, UnknownReason: replay.ReasonSnapshotMissing,
			}
			if got := unknownReasonOf(cur, pred); got != tc.cur {
				t.Errorf("unknownReasonOf() = %q, want %q —— 归属层算出来的分类被求值引擎的占位值盖掉了",
					got, tc.cur)
			}
		})
	}
}

// 归属层没给原因时仍然取候选回放那一侧：那时它是唯一有话说的一侧。
func TestThePredictedReasonIsUsedWhenAttributionHasNone(t *testing.T) {
	cur := replay.Decision{Verdict: replay.VerdictUnknown, UnknownReason: replay.ReasonNone}
	pred := replay.Decision{Verdict: replay.VerdictUnknown, UnknownReason: replay.ReasonPolicyMalformed}
	if got := unknownReasonOf(cur, pred); got != replay.ReasonPolicyMalformed {
		t.Errorf("unknownReasonOf() = %q, want POLICY_MALFORMED", got)
	}
}

// **当前判定得出结论、候选策略把它变成 UNKNOWN 时，原因必须来自候选那一侧。**
//
// 这是原设计要守的那一条：这类 UNKNOWN 是候选集自己引入的，指向的子系统
// 是策略生成，不是归属。上面那条修改不得把它一起改掉。
func TestACandidateIntroducedUnknownKeepsItsOwnReason(t *testing.T) {
	cur := replay.Decision{Verdict: replay.VerdictAllow}
	pred := replay.Decision{
		Verdict: replay.VerdictUnknown, UnknownReason: replay.ReasonPolicyMalformed,
	}
	if got := unknownReasonOf(cur, pred); got != replay.ReasonPolicyMalformed {
		t.Errorf("unknownReasonOf() = %q, want POLICY_MALFORMED —— 候选集引入的缺口该指向策略生成", got)
	}
}

// 两侧都有结论时没有原因可报。
func TestNoReasonWhenNeitherSideIsUnknown(t *testing.T) {
	cur := replay.Decision{Verdict: replay.VerdictAllow}
	pred := replay.Decision{Verdict: replay.VerdictDeny}
	if got := unknownReasonOf(cur, pred); got != replay.ReasonNone {
		t.Errorf("unknownReasonOf() = %q, want 空", got)
	}
}
