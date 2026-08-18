package policygen

import (
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/replay"
)

func pod(cluster, ns, name, app string) *replay.PodRef {
	labels := map[string]string{}
	if app != "" {
		labels["app"] = app
	}
	return &replay.PodRef{ClusterID: cluster, Namespace: ns, Name: name, Labels: labels}
}

func obs(src, dst *replay.PodRef, dstIP string, port int32, d replay.Decision) Observation {
	f := replay.Flow{
		Source:   replay.Endpoint{IP: "10.4.0.1", Pod: src},
		Dest:     replay.Endpoint{IP: dstIP, Pod: dst},
		Protocol: replay.ProtocolTCP, Port: port,
		Timestamp: time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
	}
	if src != nil {
		f.Source.ClusterID = src.ClusterID
	}
	if dst != nil {
		f.Dest.ClusterID = dst.ClusterID
	}
	// IdentityTrusted 默认 true：这些用例验的是表达能力与拆分，不是身份可信度。
	// 零值 false 的含义是"mesh / CCNP 让身份不可信"，那条由专门的用例覆盖。
	return Observation{FlowID: "f1", Flow: f, Decision: d, IdentityTrusted: true}
}

// classifyOne 按这一条观测自身推出的归属键赢家做分类，等价于 Generate
// 在名册为空、只有这一条流量时的行为——用例关心的是拆分与表达能力，
// 不是同名 workload 的归属键竞争（那条在 generate_test.go 里单独钉住）。
func classifyOne(o Observation, clusterID string) ([]keyed, []UngeneratableItem) {
	return classify(o, clusterID, resolveWinningKeys(Input{
		ClusterID: clusterID, Observations: []Observation{o},
	}))
}

func allowDecision() replay.Decision {
	return replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted}
}

// 一条集群内流量对本集群同时产出源侧 egress 与目的侧 ingress 两条聚合项。
// 少一侧就会生成单向策略：源放行了、目的没放行，上线即断。
func TestClassifyProducesBothDirectionsForInClusterFlow(t *testing.T) {
	o := obs(pod("c1", "gateway", "gateway-1", "gateway"),
		pod("c1", "payment", "payment-1", "api"), "10.4.0.4", 8080, allowDecision())
	items, bad := classifyOne(o, "c1")
	if len(bad) != 0 {
		t.Fatalf("ungeneratable = %+v, want none", bad)
	}
	if len(items) != 2 {
		t.Fatalf("aggregated items = %d, want 2 (egress + ingress)", len(items))
	}
	dirs := map[replay.Direction]bool{}
	for _, it := range items {
		dirs[it.key.Direction] = true
		if it.key.Evidence != EvidenceTrustedAllow {
			t.Errorf("evidence = %q, want TRUSTED_ALLOW", it.key.Evidence)
		}
	}
	if !dirs[replay.DirectionEgress] || !dirs[replay.DirectionIngress] {
		t.Errorf("directions = %v, want both", dirs)
	}
}

// 当前被拦下的连接分到 TRUSTED_DENY，不与正常调用混为一谈。
func TestClassifyMarksDeniedFlowsAsTrustedDeny(t *testing.T) {
	d := replay.Decision{Verdict: replay.VerdictDeny, Confidence: replay.ConfidenceTrusted}
	items, _ := classifyOne(obs(
		pod("c1", "batch", "batch-1", "worker"),
		pod("c1", "payment", "payment-1", "api"), "10.4.0.4", 3306, d), "c1")
	for _, it := range items {
		if it.key.Evidence != EvidenceTrustedDeny {
			t.Errorf("evidence = %q, want TRUSTED_DENY", it.key.Evidence)
		}
	}
}

// 出公网只有源侧一条 egress，对端用 ipBlock 表达。
func TestClassifyInternetEgressProducesSingleIPBlockItem(t *testing.T) {
	o := obs(pod("c1", "batch", "batch-1", "worker"), nil, "203.0.113.10", 22, allowDecision())
	o.Flow.Dest.ClusterID = ""
	items, _ := classifyOne(o, "c1")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (egress only)", len(items))
	}
	it := items[0]
	if it.key.Direction != replay.DirectionEgress {
		t.Errorf("direction = %q, want EGRESS", it.key.Direction)
	}
	if it.key.Evidence != EvidenceInternetEgress {
		t.Errorf("evidence = %q, want INTERNET_EGRESS", it.key.Evidence)
	}
	if it.key.PeerCIDR != "203.0.113.10/32" {
		t.Errorf("peer CIDR = %q, want 203.0.113.10/32", it.key.PeerCIDR)
	}
}

// 源 Pod 没有 app 标签时，两个方向都表达不了：egress 卡在主体侧（它自己
// 就是主体，没有标签就没有 selector），ingress 卡在对端侧（它是对端，
// 而集群内对端没有标签不能退化成 /32 ipBlock —— 见 peerOf 的注释：Pod
// 重建后 IP 会被别的 workload 复用，静默放行就成了看起来正确、实际
// 错误的规则）。两条缺口都要报，少报一条就等于漏统计了一半的覆盖缺口。
func TestClassifyReportsPodWithoutAppLabel(t *testing.T) {
	o := obs(pod("c1", "legacy", "legacy-unlabelled", ""),
		pod("c1", "payment", "payment-1", "api"), "10.4.0.4", 8080, allowDecision())
	items, bad := classifyOne(o, "c1")
	if len(items) != 0 {
		t.Errorf("items = %+v, want none; neither direction is expressible", items)
	}
	if len(bad) != 2 {
		t.Fatalf("ungeneratable = %+v, want 2 (subject-side egress + peer-side ingress)", bad)
	}
	for _, b := range bad {
		if b.Reason != ReasonNoWorkloadLabel {
			t.Errorf("reason = %q, want NO_WORKLOAD_LABEL", b.Reason)
		}
		if !strings.Contains(b.Detail, "legacy/legacy-unlabelled") {
			t.Errorf("detail = %q, want it to name legacy/legacy-unlabelled", b.Detail)
		}
	}
}

// 对照上一测试：对端在集群外时，/32 ipBlock 分支原样保留 —— 这些地址不受
// 本平台 Pod 生命周期管理，重建不会把这个 IP 复用给集群内别的 workload，
// 所以退化成 ipBlock 是安全的，和集群内无标签 Pod 的情形不是一回事。
func TestClassifyExternalPeerKeepsIPBlockFallback(t *testing.T) {
	o := obs(pod("c1", "checkout", "checkout-1", "checkout"), nil, "198.51.100.7", 443, allowDecision())
	o.Flow.Dest.ClusterID = ""
	items, bad := classifyOne(o, "c1")
	if len(bad) != 0 {
		t.Fatalf("ungeneratable = %+v, want none", bad)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (egress only)", len(items))
	}
	it := items[0]
	if it.key.Direction != replay.DirectionEgress {
		t.Errorf("direction = %q, want EGRESS", it.key.Direction)
	}
	if it.key.PeerCIDR != "198.51.100.7/32" {
		t.Errorf("peer CIDR = %q, want 198.51.100.7/32", it.key.PeerCIDR)
	}
	if it.key.PeerWorkload != "" {
		t.Errorf("peer workload = %q, want empty; an ipBlock peer has no selector", it.key.PeerWorkload)
	}
}

// 身份不可信不得作为推荐依据（spec §6.4），整条流量排除并报原因。
//
// **2026-08-18 收窄**：原先的判据是「Confidence 为 DEGRADED 就排除」，而那个
// 字段压着两件事 —— mesh / CCNP 让身份不可信，与窗口证明不了完整。后者的规则
// 本身没错，只是可能不够，现在走 EvidenceIncompleteWindow 生成、默认不启用
// （design doc 2026-08-18-learn-from-incomplete-evidence §2）。
//
// 这一条守的仍然是前者，而且必须继续守：身份不可信时学出的规则会挂到**错的
// 主体**上 —— 那不是"证据不够"，是"证据指向错的对象"。
func TestClassifyRejectsUntrustworthyIdentity(t *testing.T) {
	d := replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceDegraded}
	o := obs(
		pod("c1", "checkout", "checkout-1", "checkout"),
		pod("c1", "payment", "payment-1", "api"), "10.4.0.4", 8080, d)
	o.IdentityTrusted = false
	items, bad := classifyOne(o, "c1")
	if len(items) != 0 {
		t.Errorf("items = %d, want 0; an untrustworthy identity must not feed recommendations", len(items))
	}
	if len(bad) != 1 || bad[0].Reason != ReasonDegradedEvidence {
		t.Errorf("ungeneratable = %+v, want one DEGRADED_EVIDENCE", bad)
	}
}

func TestClassifyRejectsUnknownVerdict(t *testing.T) {
	d := replay.Decision{Verdict: replay.VerdictUnknown, Confidence: replay.ConfidenceTrusted,
		UnknownReason: replay.ReasonSnapshotMissing}
	items, bad := classifyOne(obs(
		pod("c1", "gateway", "gateway-1", "gateway"), nil, "10.4.9.9", 8080, d), "c1")
	if len(items) != 0 {
		t.Errorf("items = %d, want 0", len(items))
	}
	if len(bad) != 1 || bad[0].Reason != ReasonIdentityUnknown {
		t.Errorf("ungeneratable = %+v, want one IDENTITY_UNKNOWN", bad)
	}
}

// hostNetwork 端点作为规则主体（subject）时不受 NetworkPolicy 管控，
// 该侧不生成规则并报原因。对端设为集群外地址，避免这条 flow 顺带触发
// 对端侧的 hostNetwork 检测，让这个用例只验证 subjectOf 那一半。
func TestClassifyReportsUnmanagedSubjectEndpoint(t *testing.T) {
	src := pod("c1", "kube-system", "kube-proxy-1", "kube-proxy")
	src.HostNetwork = true
	o := obs(src, nil, "203.0.113.5", 8080, allowDecision())
	o.Flow.Dest.ClusterID = ""
	items, bad := classifyOne(o, "c1")
	if len(bad) != 1 || bad[0].Reason != ReasonUnmanagedEndpoint {
		t.Errorf("ungeneratable = %+v, want one UNMANAGED_ENDPOINT", bad)
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want none; hostNetwork subject can't express egress "+
			"and the peer is external so there is no ingress side to attempt", items)
	}
}

// hostNetwork 端点作为对端（peer）时同样不受管控：podSelector 选不中走
// 宿主机网络的 Pod，因为它的流量以节点 IP 出现。peerOf 必须和 subjectOf
// 一样拒绝它，而不是照常退化成 selector —— 否则生成的是一条谁都匹配不到
// 的幽灵规则，还会被分类成 TRUSTED_ALLOW、被 Task 6 标 Enabled=true，
// 看着正常实则空转，直接进入默认推荐策略集。
//
// 与 TestClassifyReportsUnmanagedSubjectEndpoint 用同一个 hostNetwork Pod
// 做源，但这里目的端在本集群内，因此这条 flow 同时触发 subject 与 peer
// 两侧的检测；本用例只断言 peer 侧的表现：不产出 ingress 项，且缺口原因
// 里有一条点名这个 hostNetwork 源作为对端不可表达。
func TestClassifyReportsUnmanagedPeerEndpoint(t *testing.T) {
	src := pod("c1", "kube-system", "kube-proxy-1", "kube-proxy")
	src.HostNetwork = true
	items, bad := classifyOne(obs(src,
		pod("c1", "payment", "payment-1", "api"), "10.4.0.4", 8080, allowDecision()), "c1")
	for _, it := range items {
		if it.key.Direction == replay.DirectionIngress {
			t.Errorf("ingress item emitted for a hostNetwork peer: %+v", it)
		}
	}
	found := false
	for _, b := range bad {
		if b.Reason == ReasonUnmanagedEndpoint && strings.Contains(b.Detail, "kube-proxy-1") {
			found = true
		}
	}
	if !found {
		t.Errorf("ungeneratable = %+v, want an UNMANAGED_ENDPOINT item naming the peer pod", bad)
	}
}

// 跨集群对端只能用 ipBlock，且证据类型必须区分出来。
func TestClassifyCrossClusterUsesIPBlock(t *testing.T) {
	o := obs(pod("c2", "partner", "partner-1", "partner"),
		pod("c1", "gateway", "gateway-1", "gateway"), "10.4.0.1", 8443, allowDecision())
	// 生产链路里这个标记由 replay.Evaluate 填；手工构造 Decision 时须显式设置。
	o.Decision.CrossCluster = true
	items, _ := classifyOne(o, "c1")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (destination side only)", len(items))
	}
	it := items[0]
	if it.key.Evidence != EvidenceCrossCluster {
		t.Errorf("evidence = %q, want CROSS_CLUSTER", it.key.Evidence)
	}
	if it.key.PeerCIDR == "" {
		t.Error("peer CIDR empty; a foreign-cluster peer is only expressible as ipBlock")
	}
}

/* ---------------------------------------------------------------------- */
/* 证据不完整的窗口（design doc 2026-08-18-learn-from-incomplete-evidence） */
/* ---------------------------------------------------------------------- */

// 窗口证明不了完整时，规则照学，但带自己的证据类别。
//
// **这是一次有代价的放宽**：漏看的连接不会进候选集，覆盖它的规则于是缺席，
// 而那条规则会被判「无流量、可收紧」。放宽之前的行为是一条都不学（三种来源
// 没有一条能自证完整，因此永远学不到）；放宽之后的行为是学了但默认不启用。
func TestClassifyLearnsFromAnIncompleteWindowWithItsOwnEvidenceClass(t *testing.T) {
	d := allowDecision()
	d.Confidence = replay.ConfidenceDegraded // 完整度传导下来的降级
	o := obs(pod("c1", "shop", "web-1", "web"), pod("c1", "payment", "api-1", "api"),
		"10.4.0.2", 8080, d)
	o.IdentityTrusted = true // 身份是准的，只是窗口证明不了完整

	items, bad := classifyOne(o, "c1")

	if len(bad) != 0 {
		t.Fatalf("ungeneratable = %+v, want none — 身份可信的观测不该因为窗口"+
			"证明不了完整而被整条丢掉", bad)
	}
	if len(items) == 0 {
		t.Fatal("no items were produced from an incomplete window")
	}
	for _, it := range items {
		if it.key.Evidence != EvidenceIncompleteWindow {
			t.Errorf("evidence = %q, want %q — 与可信证据混成一类，界面上就分不出"+
				"哪些规则的证据可能不全", it.key.Evidence, EvidenceIncompleteWindow)
		}
	}
}

// **身份不可信的一条都不许学。** mesh / CCNP 之后源地址不代表真实主体，
// 学出的规则会挂到错的主体上 —— 那不是"证据不够"，是"证据指向错的对象"。
func TestClassifyStillRefusesFlowsWhoseIdentityIsUntrustworthy(t *testing.T) {
	d := allowDecision()
	d.Confidence = replay.ConfidenceDegraded
	o := obs(pod("c1", "shop", "web-1", "web"), pod("c1", "payment", "api-1", "api"),
		"10.4.0.2", 8080, d)
	o.IdentityTrusted = false // 求值引擎自己因为 mesh / CCNP 降的级

	items, bad := classifyOne(o, "c1")

	if len(items) != 0 {
		t.Errorf("items = %+v, want none — 身份不可信的流量被学成了规则", items)
	}
	if len(bad) != 1 || bad[0].Reason != ReasonDegradedEvidence {
		t.Errorf("ungeneratable = %+v, want one DEGRADED_EVIDENCE", bad)
	}
}

// 完整窗口的行为一个字节不变。
func TestClassifyKeepsTrustedAllowOnACompleteWindow(t *testing.T) {
	o := obs(pod("c1", "shop", "web-1", "web"), pod("c1", "payment", "api-1", "api"),
		"10.4.0.2", 8080, allowDecision())
	o.IdentityTrusted = true

	items, bad := classifyOne(o, "c1")
	if len(bad) != 0 {
		t.Fatalf("ungeneratable = %+v, want none", bad)
	}
	for _, it := range items {
		if it.key.Evidence != EvidenceTrustedAllow {
			t.Errorf("evidence = %q, want %q on a complete window", it.key.Evidence, EvidenceTrustedAllow)
		}
	}
}
