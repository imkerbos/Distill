package policygen

import (
	"encoding/json"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/replay"
)

// persistedRule 是一条学到的规则落库时的形状。
//
// **显式列字段，不 marshal 整个 Rule。** Rule 里真正会变成 NetworkPolicy 的
// 两个字段是 json:"-"：
//
//	Ingress *networkingv1.NetworkPolicyIngressRule `json:"-"`
//	Egress  *networkingv1.NetworkPolicyEgressRule  `json:"-"`
//
// 而 Peers/Ports 是**有损**的展示串（见 FingerprintOf 的注释：展示串为了给人
// 看做了简化，describeSelector 命中 workloadLabelKeys 时只留标签值）。直接
// json.Marshal(Rule) 存下来的，是一份看着完整、实际重建不出策略的东西——
// 而它的失败方式是渲染出一条谁都不放行的策略，朝切断的方向错。
type persistedRule struct {
	Origin      RuleOrigin                             `json:"origin"`
	Evidence    EvidenceClass                          `json:"evidence,omitempty"`
	Baseline    *baseline.Kind                         `json:"baseline,omitempty"`
	Direction   replay.Direction                       `json:"direction"`
	Ingress     *networkingv1.NetworkPolicyIngressRule `json:"ingress,omitempty"`
	Egress      *networkingv1.NetworkPolicyEgressRule  `json:"egress,omitempty"`
	Derivations []baseline.Derivation                  `json:"derivations,omitempty"`
}

// MarshalRule 把一条规则序列化成可落库的字节。
//
// **三个字段刻意不存**，各有理由：
//
//   - Enabled 是判定的产物，不是规则的属性。存下来等于把某一次的证据状态与
//     人工决定冻进规则里，下次读出来就绕过了当次判定。
//   - FlowCount 是单窗口计数。读回时应当取 rule_evidence.observations 那个
//     累积值，存一个旧窗口的数只会与它打架，而两个都显示在界面上时没人
//     知道该信哪个。
//   - Risk 由端口查风险清单得出，是纯函数。存下来的那份会在清单更新之后过期，
//     而过期的方向是"这个端口不再算风险"——正是不能出的那一侧。
//
// 三者都在读回时重新算，见 UnmarshalRule 的调用方。
func MarshalRule(r Rule) ([]byte, error) {
	raw, err := json.Marshal(persistedRule{
		Origin:      r.Origin,
		Evidence:    r.Evidence,
		Baseline:    r.Baseline,
		Direction:   r.Direction,
		Ingress:     r.Ingress,
		Egress:      r.Egress,
		Derivations: r.Derivations,
	})
	if err != nil {
		return nil, fmt.Errorf("policygen: marshal rule: %w", err)
	}
	return raw, nil
}

// UnmarshalRule 从落库的字节还原一条规则。
//
// 还原出来的 Rule **不带** Enabled / FlowCount / Risk / Peers / Ports：
// 前三个见 MarshalRule 的注释，后两个是展示串，由调用方按当下的资产重新渲染
// ——一份两天前的展示串描述的是两天前的标签。
//
// Fingerprint 也不还原，由调用方对还原出来的规则重新求。这样"存进去的和
// 取出来的是同一条规则"就成了一条可以被测出来的性质，而不是一句承诺：
// 指纹只覆盖 Origin/Evidence/Direction/Baseline 与规则体，恰好就是这里存的
// 那几项，因此 round-trip 之后指纹必须逐字节相同。
func UnmarshalRule(raw []byte) (Rule, error) {
	var p persistedRule
	if err := json.Unmarshal(raw, &p); err != nil {
		return Rule{}, fmt.Errorf("policygen: unmarshal rule: %w", err)
	}
	// 认不出的取值整条报错，**不放行**：一条来源或方向说不清的规则，渲染出来
	// 仍然是一条会被下发的策略。
	switch p.Origin {
	case OriginBaseline, OriginLearned, OriginImported:
	default:
		return Rule{}, fmt.Errorf("policygen: stored rule carries an unregistered origin %q", p.Origin)
	}
	switch p.Direction {
	case replay.DirectionIngress, replay.DirectionEgress:
	default:
		return Rule{}, fmt.Errorf("policygen: stored rule carries an unregistered direction %q", p.Direction)
	}
	// 方向与规则体必须对得上：一条 INGRESS 却只带 Egress 体的记录，渲染出来
	// 是一条没有任何规则的策略，也就是一条 default-deny。宁可整条报错。
	switch p.Direction {
	case replay.DirectionIngress:
		if p.Ingress == nil {
			return Rule{}, fmt.Errorf("policygen: stored INGRESS rule has no ingress body")
		}
	case replay.DirectionEgress:
		if p.Egress == nil {
			return Rule{}, fmt.Errorf("policygen: stored EGRESS rule has no egress body")
		}
	}
	return Rule{
		Origin:      p.Origin,
		Evidence:    p.Evidence,
		Baseline:    p.Baseline,
		Direction:   p.Direction,
		Ingress:     p.Ingress,
		Egress:      p.Egress,
		Derivations: p.Derivations,
	}, nil
}
