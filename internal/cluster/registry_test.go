package cluster_test

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/imkerbos/Distill/internal/cluster"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	ps := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("parse prefix %q: %v", s, err)
		}
		ps = append(ps, p)
	}
	return ps
}

// completeFleet 是一份三类网段都登记齐的两集群 Fleet，两集群互不重叠。
func completeFleet(t *testing.T) []cluster.Cluster {
	t.Helper()
	return []cluster.Cluster{
		{
			ID:           "cluster-a",
			PodCIDRs:     prefixes(t, "10.4.0.0/16"),
			ServiceCIDRs: prefixes(t, "10.8.0.0/20"),
			NodeCIDRs:    prefixes(t, "10.128.0.0/20"),
		},
		{
			ID:           "cluster-b",
			PodCIDRs:     prefixes(t, "10.5.0.0/16"),
			ServiceCIDRs: prefixes(t, "10.9.0.0/20"),
			NodeCIDRs:    prefixes(t, "10.129.0.0/20"),
		},
	}
}

func classify(t *testing.T, r *cluster.Registry, ip string) cluster.Classification {
	t.Helper()
	got, err := r.Classify(ip)
	if err != nil {
		t.Fatalf("Classify(%s) = error %v, want no error", ip, err)
	}
	return got
}

func TestClassifyRecognizesEachCIDRClass(t *testing.T) {
	r := cluster.NewRegistry(completeFleet(t))

	cases := []struct {
		name      string
		ip        string
		wantScope cluster.Scope
		wantID    string
	}{
		{"pod of cluster-a", "10.4.1.7", cluster.ScopePod, "cluster-a"},
		{"service of cluster-a", "10.8.0.10", cluster.ScopeService, "cluster-a"},
		{"node of cluster-a", "10.128.0.3", cluster.ScopeNode, "cluster-a"},
		{"pod of cluster-b", "10.5.9.9", cluster.ScopePod, "cluster-b"},
		{"service of cluster-b", "10.9.0.10", cluster.ScopeService, "cluster-b"},
		{"node of cluster-b", "10.129.0.3", cluster.ScopeNode, "cluster-b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(t, r, c.ip)
			if got.Scope != c.wantScope {
				t.Errorf("Classify(%s).Scope = %q, want %q", c.ip, got.Scope, c.wantScope)
			}
			if got.ClusterID != c.wantID {
				t.Errorf("Classify(%s).ClusterID = %q, want %q", c.ip, got.ClusterID, c.wantID)
			}
			if got.Reason != "" {
				t.Errorf("Classify(%s).Reason = %q, want empty on a resolved verdict", c.ip, got.Reason)
			}
		})
	}
}

// IPv6 Pod 网段同样要判得出：只按 IPv4 写比对会让双栈集群的 Pod 全部落到
// EXTERNAL 上，而那是一个具体的错误结论，不是一次弃权。
func TestClassifyRecognizesIPv6Prefixes(t *testing.T) {
	r := cluster.NewRegistry([]cluster.Cluster{{
		ID:           "cluster-v6",
		PodCIDRs:     prefixes(t, "fd12:3456:789a::/64"),
		ServiceCIDRs: prefixes(t, "fd12:3456:789b::/112"),
		NodeCIDRs:    prefixes(t, "fd12:3456:789c::/64"),
	}})

	if got := classify(t, r, "fd12:3456:789a::5"); got.Scope != cluster.ScopePod {
		t.Errorf("Classify(fd12:3456:789a::5).Scope = %q, want POD", got.Scope)
	}
	if got := classify(t, r, "fd12:3456:789b::1"); got.Scope != cluster.ScopeService {
		t.Errorf("Classify(fd12:3456:789b::1).Scope = %q, want SERVICE", got.Scope)
	}
}

// 一个集群登记了多段 Pod 网段时，后追加的那段不得被漏掉 —— 扩容之后
// 新网段里的 Pod 会整批变成判不出归属。
func TestClassifyMatchesEveryRegisteredPrefixOfACluster(t *testing.T) {
	r := cluster.NewRegistry([]cluster.Cluster{{
		ID:           "cluster-a",
		PodCIDRs:     prefixes(t, "10.4.0.0/16", "10.60.0.0/16"),
		ServiceCIDRs: prefixes(t, "10.8.0.0/20"),
		NodeCIDRs:    prefixes(t, "10.128.0.0/20"),
	}})

	for _, ip := range []string{"10.4.0.1", "10.60.0.1"} {
		got := classify(t, r, ip)
		if got.Scope != cluster.ScopePod || got.ClusterID != "cluster-a" {
			t.Errorf("Classify(%s) = {%q %q}, want {POD cluster-a}", ip, got.Scope, got.ClusterID)
		}
	}
}

// 网段重叠时必须拒绝作答：任选其一会让上层 join 到另一个集群的 Pod 上，
// 而那次查询仍然返回结果、仍然不报错（spec §2.2）。
func TestClassifyRefusesToPickAmongOverlappingClusters(t *testing.T) {
	overlapping := []cluster.Cluster{
		{
			ID:           "cluster-a",
			PodCIDRs:     prefixes(t, "10.4.0.0/16"),
			ServiceCIDRs: prefixes(t, "10.8.0.0/20"),
			NodeCIDRs:    prefixes(t, "10.128.0.0/20"),
		},
		{
			ID:           "cluster-c",
			PodCIDRs:     prefixes(t, "10.4.0.0/16"),
			ServiceCIDRs: prefixes(t, "10.10.0.0/20"),
			NodeCIDRs:    prefixes(t, "10.130.0.0/20"),
		},
	}
	got := classify(t, cluster.NewRegistry(overlapping), "10.4.1.7")

	if got.Scope != cluster.ScopeAmbiguous {
		t.Errorf("Classify(10.4.1.7).Scope = %q, want AMBIGUOUS", got.Scope)
	}
	if got.ClusterID != "" {
		t.Errorf("Classify(10.4.1.7).ClusterID = %q, want empty: AMBIGUOUS must not name a winner", got.ClusterID)
	}
	for _, want := range []string{"cluster-a", "cluster-c"} {
		if !slices.Contains(got.Matches, want) {
			t.Errorf("Classify(10.4.1.7).Matches = %v, want it to contain %q", got.Matches, want)
		}
	}
}

// 重叠发生在不同类别的网段之间（一集群的 Pod 段 = 另一集群的 Node 段）
// 同样是重叠。先命中哪一类不构成"更可信"，仍然不得作答。
func TestClassifyRefusesWhenOverlapCrossesCIDRClasses(t *testing.T) {
	r := cluster.NewRegistry([]cluster.Cluster{
		{
			ID:           "cluster-a",
			PodCIDRs:     prefixes(t, "10.4.0.0/16"),
			ServiceCIDRs: prefixes(t, "10.8.0.0/20"),
			NodeCIDRs:    prefixes(t, "10.128.0.0/20"),
		},
		{
			ID:           "cluster-d",
			PodCIDRs:     prefixes(t, "10.6.0.0/16"),
			ServiceCIDRs: prefixes(t, "10.11.0.0/20"),
			NodeCIDRs:    prefixes(t, "10.4.0.0/16"),
		},
	})
	got := classify(t, r, "10.4.1.7")

	if got.Scope != cluster.ScopeAmbiguous {
		t.Errorf("Classify(10.4.1.7).Scope = %q, want AMBIGUOUS", got.Scope)
	}
	if got.ClusterID != "" {
		t.Errorf("Classify(10.4.1.7).ClusterID = %q, want empty", got.ClusterID)
	}
}

// EXTERNAL 是一个结论，只有在登记完整时才允许下。
func TestClassifyReturnsExternalOnlyWhenRegistrationIsComplete(t *testing.T) {
	got := classify(t, cluster.NewRegistry(completeFleet(t)), "8.8.8.8")

	if got.Scope != cluster.ScopeExternal {
		t.Errorf("Classify(8.8.8.8).Scope = %q, want EXTERNAL", got.Scope)
	}
	if got.ClusterID != "" || got.Reason != "" {
		t.Errorf("Classify(8.8.8.8) = %+v, want ClusterID and Reason empty", got)
	}
}

// 同一个地址的两种写法必须给出同一个结论。
//
// 4-in-6 是 IPv4 地址的另一种写法，不是另一个地址（安全规范补充版 §26），
// 而 netip.Prefix.Contains 不会自己拆包装。危险方向不是"判错了集群"：
// 登记不全时 ::ffff:10.4.1.7 落到 UNKNOWN，那是一次弃权，失败方向是安全的；
// 登记完整时它落到 EXTERNAL —— 一个下游会当作事实使用的结论，
// 而且是"这个地址在 Fleet 外"这种最让人放心的错误方向。
func TestClassifyGivesOneVerdictForBothSpellingsOfTheSameAddress(t *testing.T) {
	gapped := completeFleet(t)
	gapped[1].NodeCIDRs = nil

	registries := []struct {
		name string
		reg  *cluster.Registry
	}{
		{"complete registration", cluster.NewRegistry(completeFleet(t))},
		{"incomplete registration", cluster.NewRegistry(gapped)},
	}
	addresses := []struct {
		name   string
		plain  string
		mapped string
	}{
		{"pod address", "10.4.1.7", "::ffff:10.4.1.7"},
		{"service address", "10.8.0.5", "::ffff:10.8.0.5"},
		{"node address", "10.128.0.3", "::ffff:10.128.0.3"},
		{"address outside every registered CIDR", "8.8.8.8", "::ffff:8.8.8.8"},
	}

	for _, r := range registries {
		for _, a := range addresses {
			t.Run(r.name+"/"+a.name, func(t *testing.T) {
				want := classify(t, r.reg, a.plain)
				got := classify(t, r.reg, a.mapped)

				if got.Scope != want.Scope || got.ClusterID != want.ClusterID ||
					got.Reason != want.Reason || !slices.Equal(got.Matches, want.Matches) {
					t.Errorf("Classify(%s) = %+v, want the same verdict as Classify(%s) = %+v",
						a.mapped, got, a.plain, want)
				}
			})
		}
	}
}

// 带 zone 的地址不作答，报错。
//
// zone 是"本机某张网卡"的作用域，拿它问全 Fleet 的网段归属没有意义，
// 而本包的规则是宁可不答。两个方向各一条：
//   - 真 IPv6 是危险的那条。Unmap 对它恒等，zone 原样留着，而
//     netip.Prefix.Contains 对带 zone 的地址一律返回 false —— 不拒绝的话，
//     一个就在本集群 Pod 网段里的地址会被判成 EXTERNAL，正是本包禁止的
//     那种确信的错误结论。
//   - 4-in-6 是相反的方向。Unmap 会顺手丢掉它的 zone，不先拒绝就等于
//     悄悄丢掉一部分输入再作答。判定顺序（先拒 zone，后 Unmap）就钉在这条上。
func TestClassifyRefusesZonedAddresses(t *testing.T) {
	r := cluster.NewRegistry([]cluster.Cluster{{
		ID:           "cluster-v6",
		PodCIDRs:     prefixes(t, "fd12:3456:789a:1::/64", "10.4.0.0/16"),
		ServiceCIDRs: prefixes(t, "fd12:3456:789a:2::/112"),
		NodeCIDRs:    prefixes(t, "fd12:3456:789a:3::/64"),
	}})

	for _, ip := range []string{"fd12:3456:789a:1::7%eth0", "::ffff:10.4.1.7%eth0"} {
		got, err := r.Classify(ip)
		if err == nil {
			t.Errorf("Classify(%q) = %+v with no error, want an error: a zoned address is not a fleet-wide question", ip, got)
		}
		if got.Scope != "" {
			t.Errorf("Classify(%q).Scope = %q on error, want empty", ip, got.Scope)
		}
	}
}

// 拆 4-in-6 包装只影响 4-in-6 写法：真正的 IPv6 地址仍按 IPv6 网段判定。
// netip.Addr.Unmap 对非 4-in-6 的地址是恒等的，这条用例是那句话的证据。
func TestClassifyStillJudgesGenuineIPv6Addresses(t *testing.T) {
	r := cluster.NewRegistry([]cluster.Cluster{{
		ID:           "cluster-v6",
		PodCIDRs:     prefixes(t, "fd12:3456:789a:1::/64"),
		ServiceCIDRs: prefixes(t, "fd12:3456:789a:2::/112"),
		NodeCIDRs:    prefixes(t, "fd12:3456:789a:3::/64"),
	}})

	if got := classify(t, r, "fd12:3456:789a:1::7"); got.Scope != cluster.ScopePod || got.ClusterID != "cluster-v6" {
		t.Errorf("Classify(fd12:3456:789a:1::7) = {%q %q}, want {POD cluster-v6}", got.Scope, got.ClusterID)
	}
	if got := classify(t, r, "2606:4700::1111"); got.Scope != cluster.ScopeExternal {
		t.Errorf("Classify(2606:4700::1111).Scope = %q, want EXTERNAL", got.Scope)
	}
}

// 登记不全时未命中的地址必须是 UNKNOWN 而不是 EXTERNAL：后者是把
// "我们没登记"讲成"它在集群外"（spec §2.1）。Reason 说明缺哪一类。
func TestClassifyRefusesWhenRegistrationIsIncomplete(t *testing.T) {
	full := completeFleet(t)

	noPod := completeFleet(t)
	noPod[1].PodCIDRs = nil
	noService := completeFleet(t)
	noService[1].ServiceCIDRs = nil
	noNode := completeFleet(t)
	noNode[1].NodeCIDRs = nil

	cases := []struct {
		name       string
		clusters   []cluster.Cluster
		wantReason cluster.Reason
	}{
		{"no cluster registered at all", nil, cluster.ReasonNoClustersRegistered},
		{"empty cluster list", []cluster.Cluster{}, cluster.ReasonNoClustersRegistered},
		{"a cluster without pod CIDRs", noPod, cluster.ReasonPodCIDRUnregistered},
		{"a cluster without service CIDRs", noService, cluster.ReasonServiceCIDRUnregistered},
		{"a cluster without node CIDRs", noNode, cluster.ReasonNodeCIDRUnregistered},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(t, cluster.NewRegistry(c.clusters), "8.8.8.8")
			if got.Scope == cluster.ScopeExternal {
				t.Fatalf("Classify(8.8.8.8).Scope = EXTERNAL with an incomplete registration, want UNKNOWN")
			}
			if got.Scope != cluster.ScopeUnknown {
				t.Fatalf("Classify(8.8.8.8).Scope = %q, want UNKNOWN", got.Scope)
			}
			if got.Reason != c.wantReason {
				t.Errorf("Classify(8.8.8.8).Reason = %q, want %q", got.Reason, c.wantReason)
			}
			if got.ClusterID != "" {
				t.Errorf("Classify(8.8.8.8).ClusterID = %q, want empty", got.ClusterID)
			}
		})
	}

	// 对照：同一批地址在完整登记下确实拿得到 EXTERNAL，否则上面几条
	// 会因为"永远不是 EXTERNAL"而白绿。
	if got := classify(t, cluster.NewRegistry(full), "8.8.8.8"); got.Scope != cluster.ScopeExternal {
		t.Errorf("Classify(8.8.8.8) with a complete registration = %q, want EXTERNAL", got.Scope)
	}
}

// 缺口只报最严重的一条，顺序 Pod → Service → Node：操作者据此知道先补哪个。
func TestRegistrationGapReportsTheMostSevereMissingCIDR(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(cs []cluster.Cluster)
		wantReason cluster.Reason
	}{
		{
			name:       "pod outranks service and node",
			mutate:     func(cs []cluster.Cluster) { cs[0].PodCIDRs, cs[0].ServiceCIDRs, cs[0].NodeCIDRs = nil, nil, nil },
			wantReason: cluster.ReasonPodCIDRUnregistered,
		},
		{
			name:       "service outranks node",
			mutate:     func(cs []cluster.Cluster) { cs[0].ServiceCIDRs, cs[0].NodeCIDRs = nil, nil },
			wantReason: cluster.ReasonServiceCIDRUnregistered,
		},
		{
			name:       "a gap in any one cluster is a gap for the whole fleet",
			mutate:     func(cs []cluster.Cluster) { cs[1].PodCIDRs = nil },
			wantReason: cluster.ReasonPodCIDRUnregistered,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := completeFleet(t)
			c.mutate(cs)
			got := classify(t, cluster.NewRegistry(cs), "8.8.8.8")
			if got.Reason != c.wantReason {
				t.Errorf("Classify(8.8.8.8).Reason = %q, want %q", got.Reason, c.wantReason)
			}
		})
	}
}

// 登记有缺口不影响已经命中的判定：缺口只解释"没命中"的成因。
func TestClassifyStillAnswersHitsWhileRegistrationIsIncomplete(t *testing.T) {
	cs := completeFleet(t)
	cs[0].ServiceCIDRs = nil
	r := cluster.NewRegistry(cs)

	got := classify(t, r, "10.4.1.7")
	if got.Scope != cluster.ScopePod || got.ClusterID != "cluster-a" {
		t.Errorf("Classify(10.4.1.7) = {%q %q}, want {POD cluster-a}", got.Scope, got.ClusterID)
	}
	if got.Reason != "" {
		t.Errorf("Classify(10.4.1.7).Reason = %q, want empty on a resolved verdict", got.Reason)
	}
}

// 判定依据一旦交出去就不再受调用方那份切片影响。
func TestNewRegistryCopiesTheClusterList(t *testing.T) {
	cs := completeFleet(t)
	r := cluster.NewRegistry(cs)

	cs[0] = cluster.Cluster{ID: "replaced", PodCIDRs: prefixes(t, "0.0.0.0/0")}
	cs = append(cs, cluster.Cluster{ID: "appended", PodCIDRs: prefixes(t, "8.8.8.0/24")})
	_ = cs

	if got := classify(t, r, "10.4.1.7"); got.ClusterID != "cluster-a" {
		t.Errorf("Classify(10.4.1.7).ClusterID = %q after mutating the caller's slice, want cluster-a", got.ClusterID)
	}
	if got := classify(t, r, "8.8.8.8"); got.Scope != cluster.ScopeExternal {
		t.Errorf("Classify(8.8.8.8).Scope = %q after appending to the caller's slice, want EXTERNAL", got.Scope)
	}
}

// 认不出的地址必须报错，不得当成 EXTERNAL 静默吞掉。
func TestClassifyRejectsMalformedAddresses(t *testing.T) {
	r := cluster.NewRegistry(completeFleet(t))
	for _, ip := range []string{"", "not-an-ip", "10.4.1.7/16", "10.4.1.7:6443", "10.4.1.256", "10.4.1.7%eth0"} {
		got, err := r.Classify(ip)
		if err == nil {
			t.Errorf("Classify(%q) = %+v with no error, want an error", ip, got)
		}
		if got.Scope != "" {
			t.Errorf("Classify(%q).Scope = %q on error, want empty", ip, got.Scope)
		}
	}
}
