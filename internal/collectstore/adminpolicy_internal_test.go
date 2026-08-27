package collectstore

import (
	"strings"
	"testing"
)

// 严格解码：认不出的字段一律报错，不静默丢弃。
//
// encoding/json 的默认行为是把不认识的字段悄悄扔掉。这一族带 Deny 且排在
// 标准 NetworkPolicy 之前，扔掉的可能正是那个把规则收窄的条件 —— 一条本该
// 只拦某个端口的 Deny 会被读成拦一切，或者反过来。
func TestParseAdminPoliciesRejectsUnknownFields(t *testing.T) {
	_, _, err := parseAdminPolicies([]observedAdminPolicy{{
		kind: adminPolicyKindAdmin, name: "a",
		manifest: `apiVersion: policy.networking.k8s.io/v1alpha1
kind: AdminNetworkPolicy
spec:
  priority: 10
  subject:
    namespaces: {}
  ingress:
  - action: Deny
    somethingWeDoNotUnderstand: true
    from:
    - namespaces: {}
`,
	}})
	if err == nil {
		t.Fatal("一个带未知字段的 ANP 被静默接受了；扔掉的那个字段可能正是收窄条件")
	}
	if !strings.Contains(err.Error(), "cannot be parsed") {
		t.Errorf("err = %v，希望它说清是解析不了", err)
	}
}

// 解析不了的原文整体报错，不跳过。
//
// 跳过一条读不懂的 ANP，它的 Deny 就此消失，那条连接会被判成放行。
func TestParseAdminPoliciesFailsWholeSetOnBadManifest(t *testing.T) {
	_, _, err := parseAdminPolicies([]observedAdminPolicy{
		{kind: adminPolicyKindAdmin, name: "good", manifest: "spec:\n  priority: 10\n"},
		{kind: adminPolicyKindAdmin, name: "bad", manifest: "spec: [this is not an object]"},
	})
	if err == nil {
		t.Fatal("一条读不懂的 ANP 被跳过了，剩下的照常返回")
	}
}

// 名字以库里那一列为准，不以 manifest 里的为准。
//
// 与 parsePolicies 对 namespace 的处理同理：库里那一列是采集当时看到的事实，
// manifest 只是原文证据，两者不一致时该信前者。
func TestParseAdminPoliciesTakesTheNameFromTheRow(t *testing.T) {
	anps, _, err := parseAdminPolicies([]observedAdminPolicy{{
		kind: adminPolicyKindAdmin, name: "real-name",
		manifest: "metadata:\n  name: stale-name\nspec:\n  priority: 10\n",
	}})
	if err != nil {
		t.Fatalf("parseAdminPolicies() = %v", err)
	}
	if len(anps) != 1 || anps[0].Name != "real-name" {
		t.Errorf("name = %q，希望取库里那一列的 real-name", anps[0].Name)
	}
}

// BANP 是集群级单例；库里出现两条说明采到的东西不是我们以为的形状。
func TestParseAdminPoliciesRefusesTwoBaselines(t *testing.T) {
	_, _, err := parseAdminPolicies([]observedAdminPolicy{
		{kind: adminPolicyKindBaseline, name: "default", manifest: "spec: {}\n"},
		{kind: adminPolicyKindBaseline, name: "other", manifest: "spec: {}\n"},
	})
	if err == nil {
		t.Fatal("两条 BANP 被一起接受了；它是集群级单例")
	}
}

// 认不出的种类不能跳过：库里出现它说明写入侧长出了新东西，而这一层
// 还不知道该怎么解释它。
func TestParseAdminPoliciesRefusesUnknownKind(t *testing.T) {
	_, _, err := parseAdminPolicies([]observedAdminPolicy{
		{kind: "SOMETHING_NEW", name: "x", manifest: "spec: {}\n"},
	})
	if err == nil {
		t.Fatal("认不出的种类被跳过了")
	}
}

// 两个种类取值必须与写入侧一致。
//
// 这两个常量是抄来的（读侧不 import 写侧的类型），抄错的症状是一条 ANP
// 被判成"认不出的种类"，而那会让整次求值报错 —— 响亮，但要有人指出来
// 该去比对哪两处。
func TestAdminPolicyKindsMatchTheWriteSide(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{adminPolicyKindAdmin, "ADMIN_NETWORK_POLICY"},
		{adminPolicyKindBaseline, "BASELINE_ADMIN_NETWORK_POLICY"},
	} {
		if tc.got != tc.want {
			t.Errorf("kind = %q, want %q —— 与 snapshot.AdminPolicyKind 对齐", tc.got, tc.want)
		}
	}
}
