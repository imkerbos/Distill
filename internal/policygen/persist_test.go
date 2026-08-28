package policygen_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
)

func tcp(port int32) []networkingv1.NetworkPolicyPort {
	p := corev1.ProtocolTCP
	v := intstr.FromInt32(port)
	return []networkingv1.NetworkPolicyPort{{Protocol: &p, Port: &v}}
}

func ingressRule() policygen.Rule {
	return policygen.Rule{
		Origin:    policygen.OriginLearned,
		Evidence:  policygen.EvidenceTrustedAllow,
		Direction: replay.DirectionIngress,
		Ingress: &networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "g32-base"},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/name": "base-client-backend"},
				},
			}},
			Ports: tcp(8848),
		},
	}
}

// **存进去取出来必须是同一条规则**，判据是指纹逐字节相同。
//
// 这条性质是整个累积规则集的地基：合并时按 (namespace, workload, fingerprint)
// 去重，人工确认也按同一个键挂靠。round-trip 之后指纹变了，等于同一条规则在
// 库里有两个身份——候选集里出现两遍，而人工确认只对其中一个生效，界面上那两行
// 长得一模一样（FingerprintOf 的注释说的正是这个失败方式）。
func TestRuleSurvivesARoundTripWithTheSameFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule policygen.Rule
	}{
		{"ingress", ingressRule()},
		{"egress", func() policygen.Rule {
			r := ingressRule()
			r.Direction = replay.DirectionEgress
			r.Ingress = nil
			r.Egress = &networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{{
					IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"},
				}},
				Ports: tcp(6379),
			}
			return r
		}()},
		{"baseline", func() policygen.Rule {
			r := ingressRule()
			k := baseline.KindDNS
			r.Origin = policygen.OriginBaseline
			r.Baseline = &k
			return r
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := policygen.MarshalRule(tc.rule)
			if err != nil {
				t.Fatalf("MarshalRule() error = %v", err)
			}
			got, err := policygen.UnmarshalRule(raw)
			if err != nil {
				t.Fatalf("UnmarshalRule() error = %v", err)
			}
			want := policygen.FingerprintOf(tc.rule)
			if fp := policygen.FingerprintOf(got); fp != want {
				t.Errorf("往返之后指纹变了:\n  存前 %s\n  取回 %s", want, fp)
			}
		})
	}
}

// 规则体是**真的**被存下来了，不是靠展示串糊过去的。
//
// Rule 里 Ingress/Egress 是 json:"-"，直接 marshal 整个结构体会把它们丢掉，
// 而丢掉之后指纹仍然可能对得上（如果指纹也只看展示串的话）——所以光验指纹
// 不够，还要验渲染成 NetworkPolicy 的那部分真的在。
func TestTheRuleBodyItselfIsPersisted(t *testing.T) {
	raw, err := policygen.MarshalRule(ingressRule())
	if err != nil {
		t.Fatalf("MarshalRule() error = %v", err)
	}
	got, err := policygen.UnmarshalRule(raw)
	if err != nil {
		t.Fatalf("UnmarshalRule() error = %v", err)
	}
	if got.Ingress == nil {
		t.Fatal("入站规则体没了 —— 渲染出来会是一条谁都不放行的策略")
	}
	if len(got.Ingress.From) != 1 || got.Ingress.From[0].PodSelector == nil {
		t.Fatalf("对端选择器没还原出来: %+v", got.Ingress.From)
	}
	if got.Ingress.From[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "base-client-backend" {
		t.Errorf("选择器内容不对: %+v", got.Ingress.From[0].PodSelector)
	}
	if len(got.Ingress.Ports) != 1 || got.Ingress.Ports[0].Port.IntValue() != 8848 {
		t.Errorf("端口没还原出来: %+v", got.Ingress.Ports)
	}
}

// 判定的产物不进库：Enabled / FlowCount / Risk 取回来必须是零值，
// 由调用方按当下重新算。存下来的那一份会在证据变化、风险清单更新之后过期，
// 而过期的方向都是"看起来更该放行"。
func TestVerdictProductsAreNotPersisted(t *testing.T) {
	r := ingressRule()
	r.Enabled = true
	r.FlowCount = 4242
	raw, err := policygen.MarshalRule(r)
	if err != nil {
		t.Fatalf("MarshalRule() error = %v", err)
	}
	if s := string(raw); strings.Contains(s, "4242") || strings.Contains(s, "enabled") {
		t.Errorf("判定产物被存进去了: %s", s)
	}
	got, err := policygen.UnmarshalRule(raw)
	if err != nil {
		t.Fatalf("UnmarshalRule() error = %v", err)
	}
	if got.Enabled || got.FlowCount != 0 || got.Risk != nil {
		t.Errorf("取回的规则带上了判定产物: enabled=%v flows=%d risk=%v",
			got.Enabled, got.FlowCount, got.Risk)
	}
}

// 认不出的记录整条报错，**不放行**：一条来源或方向说不清、或者方向与规则体
// 对不上的记录，渲染出来仍然是一条会被下发的策略。
func TestUnrecognisedStoredRulesAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"不是 JSON", "{"},
		{"来源认不出", `{"origin":"GUESSED","direction":"INGRESS","ingress":{}}`},
		{"方向认不出", `{"origin":"LEARNED","direction":"SIDEWAYS","ingress":{}}`},
		{"说是入站却只有出站体", `{"origin":"LEARNED","direction":"INGRESS","egress":{}}`},
		{"说是出站却只有入站体", `{"origin":"LEARNED","direction":"EGRESS","ingress":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := policygen.UnmarshalRule([]byte(tc.raw)); err == nil {
				t.Error("被接受了 —— 它会变成一条下发出去的策略")
			}
		})
	}
}
