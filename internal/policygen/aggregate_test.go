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
	return Observation{FlowID: "f1", Flow: f, Decision: d}
}

func allowDecision() replay.Decision {
	return replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted}
}

// 一条集群内流量对本集群同时产出源侧 egress 与目的侧 ingress 两条聚合项。
// 少一侧就会生成单向策略：源放行了、目的没放行，上线即断。
func TestClassifyProducesBothDirectionsForInClusterFlow(t *testing.T) {
	o := obs(pod("c1", "gateway", "gateway-1", "gateway"),
		pod("c1", "payment", "payment-1", "api"), "10.4.0.4", 8080, allowDecision())
	items, bad := classify(o, "c1")
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
	items, _ := classify(obs(
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
	items, _ := classify(o, "c1")
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
	items, bad := classify(o, "c1")
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
	items, bad := classify(o, "c1")
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

// DEGRADED 不得作为推荐依据（spec §6.4），整条流量排除并报原因。
func TestClassifyRejectsDegradedEvidence(t *testing.T) {
	d := replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceDegraded}
	items, bad := classify(obs(
		pod("c1", "checkout", "checkout-1", "checkout"),
		pod("c1", "payment", "payment-1", "api"), "10.4.0.4", 8080, d), "c1")
	if len(items) != 0 {
		t.Errorf("items = %d, want 0; DEGRADED must not feed recommendations", len(items))
	}
	if len(bad) != 1 || bad[0].Reason != ReasonDegradedEvidence {
		t.Errorf("ungeneratable = %+v, want one DEGRADED_EVIDENCE", bad)
	}
}

func TestClassifyRejectsUnknownVerdict(t *testing.T) {
	d := replay.Decision{Verdict: replay.VerdictUnknown, Confidence: replay.ConfidenceTrusted,
		UnknownReason: replay.ReasonSnapshotMissing}
	items, bad := classify(obs(
		pod("c1", "gateway", "gateway-1", "gateway"), nil, "10.4.9.9", 8080, d), "c1")
	if len(items) != 0 {
		t.Errorf("items = %d, want 0", len(items))
	}
	if len(bad) != 1 || bad[0].Reason != ReasonIdentityUnknown {
		t.Errorf("ungeneratable = %+v, want one IDENTITY_UNKNOWN", bad)
	}
}

// hostNetwork 端点不受 NetworkPolicy 管控，该侧不生成规则并报原因。
func TestClassifyReportsUnmanagedEndpoint(t *testing.T) {
	src := pod("c1", "kube-system", "kube-proxy-1", "kube-proxy")
	src.HostNetwork = true
	items, bad := classify(obs(src,
		pod("c1", "payment", "payment-1", "api"), "10.4.0.4", 8080, allowDecision()), "c1")
	if len(bad) != 1 || bad[0].Reason != ReasonUnmanagedEndpoint {
		t.Errorf("ungeneratable = %+v, want one UNMANAGED_ENDPOINT", bad)
	}
	if len(items) != 1 || items[0].key.Direction != replay.DirectionIngress {
		t.Errorf("items = %+v, want only the destination-side ingress item", items)
	}
}

// 跨集群对端只能用 ipBlock，且证据类型必须区分出来。
func TestClassifyCrossClusterUsesIPBlock(t *testing.T) {
	o := obs(pod("c2", "partner", "partner-1", "partner"),
		pod("c1", "gateway", "gateway-1", "gateway"), "10.4.0.1", 8443, allowDecision())
	// 生产链路里这个标记由 replay.Evaluate 填；手工构造 Decision 时须显式设置。
	o.Decision.CrossCluster = true
	items, _ := classify(o, "c1")
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
