package baseline_test

import (
	"errors"
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/replay"
)

// 枚举必须有唯一登记处。漏登记的取值会在 Missing() 里被当成
// "不必备"而静默跳过，六类齐备的门禁随之失效。
func TestAllKindsRegistersExactlySix(t *testing.T) {
	kinds := baseline.AllKinds()
	if len(kinds) != 6 {
		t.Fatalf("AllKinds() returned %d kinds, want 6", len(kinds))
	}
	want := map[baseline.Kind]bool{
		baseline.KindDNS: false, baseline.KindLBHealth: false,
		baseline.KindMetrics: false, baseline.KindControlPlane: false,
		baseline.KindNodeAgent: false, baseline.KindExposedIngress: false,
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

// 长度对比抓不住"漏登记"：如果新增的 Kind 常量既没写进 allKinds，
// 也没写进这里的 want，两处同时少一个，长度依然相等，测试照样通过。
// 这里改为对每一个已声明的常量显式调用 Valid()——新增常量时按惯例
// 把它加进下面的列表，若同时忘了登记进 allKinds，这里就会失败，
// 而不是被"长度凑巧相等"悄悄放过。
func TestEveryDeclaredKindIsValid(t *testing.T) {
	for _, k := range []baseline.Kind{
		baseline.KindDNS,
		baseline.KindLBHealth,
		baseline.KindMetrics,
		baseline.KindControlPlane,
		baseline.KindNodeAgent,
		baseline.KindExposedIngress,
	} {
		if !k.Valid() {
			t.Errorf("declared kind %q is not registered in allKinds", k)
		}
	}
}

// EXPOSED_INGRESS 必须进封闭枚举。
//
// 漏登记的后果不是编译错误：Valid 会拒绝它，Missing 不会把它算进齐备性
// 校验，于是一个「入口没有放行规则」的集群在缺失清单上看起来是齐备的。
func TestExposedIngressIsRegistered(t *testing.T) {
	if !baseline.KindExposedIngress.Valid() {
		t.Error("KindExposedIngress 不在封闭枚举里")
	}
	if !slices.Contains(baseline.AllKinds(), baseline.KindExposedIngress) {
		t.Errorf("AllKinds() 少了 KindExposedIngress: %v", baseline.AllKinds())
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
