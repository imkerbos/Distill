package fleet_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/fleet"
	"github.com/imkerbos/Distill/internal/registry"
)

// dualCluster 是一条双栈登记：Pod 与 Node 各有一个 IPv4、一个 IPv6 网段。
func dualCluster() registry.Cluster {
	return registry.Cluster{
		ID:       "dual",
		PodCIDR:  "10.4.0.0/14, fd00:10:4::/56",
		NodeCIDR: "10.128.0.0/20,fd00:10:128::/64",
	}
}

// **双栈集群的两个网段都要认。**
//
// 一个 Pod 在双栈集群里有两个地址。只登记得下一个的话，走另一个协议族的
// 连接会落进 EXTERNAL —— 平台把它当成出公网，于是生成一条 ipBlock 规则
// 而不是 selector 规则，放行面比实际需要的宽得多。
func TestDualStackRegistrationClassifiesBothFamilies(t *testing.T) {
	reg, unusable := fleet.FromRegistry([]registry.Cluster{dualCluster()})
	if len(unusable) != 0 {
		t.Fatalf("双栈登记被判成用不了：%v", unusable)
	}

	for _, tc := range []struct {
		ip   string
		want cluster.Scope
	}{
		{"10.4.1.7", cluster.ScopePod},
		{"fd00:10:4::7", cluster.ScopePod},
		{"10.128.0.9", cluster.ScopeNode},
		{"fd00:10:128::9", cluster.ScopeNode},
	} {
		got, err := reg.Classify(tc.ip)
		if err != nil {
			t.Errorf("Classify(%s) = %v", tc.ip, err)
			continue
		}
		if got.Scope != tc.want {
			t.Errorf("Classify(%s).Scope = %s, want %s", tc.ip, got.Scope, tc.want)
		}
		if got.ClusterID != "dual" {
			t.Errorf("Classify(%s).ClusterID = %q, want dual", tc.ip, got.ClusterID)
		}
	}
}

// 单个网段的登记照旧工作：绝大多数集群是单栈，不能因为支持多段就要求他们改写。
func TestSingleCIDRRegistrationStillWorks(t *testing.T) {
	reg, unusable := fleet.FromRegistry([]registry.Cluster{{
		ID: "single", PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
	}})
	if len(unusable) != 0 {
		t.Fatalf("单段登记被判成用不了：%v", unusable)
	}
	got, err := reg.Classify("10.4.1.7")
	if err != nil || got.Scope != cluster.ScopePod {
		t.Errorf("Classify() = %+v, %v", got, err)
	}
}

// **一段写错就整条登记作废，不是"能用几段算几段"。**
//
// 部分可用的登记会让一部分地址落进 UNKNOWN，而运维看到的是"大部分都对"——
// 那条写错的网段要等到某条流量归属错了才暴露。整条作废会立刻出现在
// 「网段登记坏掉」的告警里。
func TestOneBadSegmentInvalidatesTheWholeRegistration(t *testing.T) {
	_, unusable := fleet.FromRegistry([]registry.Cluster{{
		ID: "half-bad", PodCIDR: "10.4.0.0/14, not-a-cidr", NodeCIDR: "10.128.0.0/20",
	}})
	if len(unusable) != 1 || unusable[0] != "half-bad" {
		t.Errorf("unusable = %v，一段写错的登记没有被整条判成用不了", unusable)
	}
}

// 空段（多余的逗号）同样作废：它多半是手滑，而静默忽略会让人以为填对了。
func TestEmptySegmentIsRejected(t *testing.T) {
	_, unusable := fleet.FromRegistry([]registry.Cluster{{
		ID: "trailing", PodCIDR: "10.4.0.0/14,", NodeCIDR: "10.128.0.0/20",
	}})
	if len(unusable) != 1 {
		t.Errorf("unusable = %v，多余逗号留下的空段被静默忽略了", unusable)
	}
}
