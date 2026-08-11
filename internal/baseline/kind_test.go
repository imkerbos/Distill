package baseline_test

import (
	"errors"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/replay"
)

// 枚举必须有唯一登记处。漏登记的取值会在 Missing() 里被当成
// "不必备"而静默跳过，五类齐备的门禁随之失效。
func TestAllKindsRegistersExactlyFive(t *testing.T) {
	kinds := baseline.AllKinds()
	if len(kinds) != 5 {
		t.Fatalf("AllKinds() returned %d kinds, want 5", len(kinds))
	}
	want := map[baseline.Kind]bool{
		baseline.KindDNS: false, baseline.KindLBHealth: false,
		baseline.KindMetrics: false, baseline.KindControlPlane: false,
		baseline.KindNodeAgent: false,
	}
	for _, k := range kinds {
		if _, known := want[k]; !known {
			t.Errorf("AllKinds() returned unregistered kind %q", k)
		}
		want[k] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("kind %q missing from AllKinds()", k)
		}
	}
}

func TestUnregisteredKindIsInvalid(t *testing.T) {
	if baseline.Kind("SOMETHING_ELSE").Valid() {
		t.Error("unregistered kind reported valid")
	}
	if !baseline.KindDNS.Valid() {
		t.Error("KindDNS reported invalid")
	}
}

// spec §7.2：Baseline 必须带推导依据。空 derivation 的规则不得构造成功 ——
// 靠类型约束兜住，不靠 review 记得。
func TestNewRuleRejectsEmptyDerivations(t *testing.T) {
	_, err := baseline.NewRule(
		baseline.KindDNS, replay.DirectionEgress,
		nil, &networkingv1.NetworkPolicyEgressRule{}, nil,
	)
	if !errors.Is(err, baseline.ErrNoDerivation) {
		t.Errorf("err = %v, want ErrNoDerivation", err)
	}
}

func TestNewRuleAcceptsRuleWithDerivation(t *testing.T) {
	r, err := baseline.NewRule(
		baseline.KindDNS, replay.DirectionEgress,
		nil, &networkingv1.NetworkPolicyEgressRule{},
		[]baseline.Derivation{{
			SourceKind: baseline.SourceService, Cluster: "c1",
			Namespace: "kube-system", Name: "kube-dns", Field: "spec.selector",
		}},
	)
	if err != nil {
		t.Fatalf("NewRule() error = %v, want nil", err)
	}
	if r.Kind != baseline.KindDNS || r.Direction != replay.DirectionEgress {
		t.Errorf("rule = %+v, want KindDNS/EGRESS", r)
	}
}
