package collectstore

import (
	"context"
	"fmt"
	"net/netip"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/store"
)

// unspecifiedUnknownReason 是 UNKNOWN 但没有对应原因时在构成明细里的归类。
//
// 取值与 internal/store 的那份逐字相同：同一个前端要渲染两个 Reader 的
// 输出，两边给不同的桶名会让同一件事在界面上分裂成两项。不计入则更糟 ——
// 明细各项之和必须等于 UnknownCount，否则运维照着一份自己少加了一项的账
// 去排查。
const unspecifiedUnknownReason = "UNSPECIFIED"

// Quality 返回一个集群在最近一次观测窗口里的数据质量。
//
// 没有可用采集时返回 ErrNoCollection，理由与 Topology 相同：一份全零的
// 质量报告读起来是"这个集群一切正常"。
//
// **本轮每一条连接的 verdict 都是 UNKNOWN**，因此 UnknownCount 等于
// TotalFlows：判定要由回放引擎给出，而它还没有接到采集数据上（design doc
// §7）。报一个更小的 UNKNOWN 数会让人以为其余那些已经判过了 —— 那是让人
// 放心的那个方向。构成明细如实说明每一条到底卡在哪里：归属不明的按
// design doc §3 的封闭枚举归类，归属清楚、只是还没判的落进 UNSPECIFIED。
func (r *Reader) Quality(ctx context.Context, clusterID string) (store.Quality, error) {
	d, err := r.describe(ctx, clusterID)
	if err != nil {
		return store.Quality{}, err
	}

	fleet, err := r.fleetRegistry(ctx)
	if err != nil {
		return store.Quality{}, err
	}

	q := store.Quality{
		Cluster:            clusterID,
		TotalFlows:         len(d.conns),
		UnknownComposition: map[string]int{},
	}
	for _, c := range d.conns {
		_, srcOutcome := d.subjectAt(c.Source)
		_, dstOutcome := d.subjectAt(c.Dest)

		q.UnknownCount++
		reason := unknownReasonFor(srcOutcome, dstOutcome, d.completeness)
		if reason == "" {
			q.UnknownComposition[unspecifiedUnknownReason]++
		} else {
			q.UnknownComposition[string(reason)]++
		}
		if crossesCluster(fleet, clusterID, c.Source.IP) || crossesCluster(fleet, clusterID, c.Dest.IP) {
			q.CrossClusterCount++
		}
	}

	// 完整度是按窗口算的，因此它对这个窗口里的每一条连接一视同仁
	// （design doc §4）。verdict 与 confidence 是两个字段，这里填的是后者。
	if d.completeness == flow.CompletenessComplete {
		q.TrustedRate = safeRate(q.TotalFlows, q.TotalFlows)
	} else {
		q.DegradedRate = safeRate(q.TotalFlows, q.TotalFlows)
	}
	q.UnknownRate = safeRate(q.UnknownCount, q.TotalFlows)

	if err := r.fillPodQuality(ctx, d, &q); err != nil {
		return store.Quality{}, err
	}
	return q, nil
}

// fillPodQuality 填上策略覆盖率与两类 Pod 计数。
//
// 这三项要拿标签比 selector，而标签只在快照里 —— 取的是覆盖那一刻的那次
// 采集，不是最新一次（CLAUDE.md §4）。
func (r *Reader) fillPodQuality(ctx context.Context, d described, q *store.Quality) error {
	pods, err := r.readPodsAt(ctx, d)
	if err != nil {
		return err
	}
	policies, err := r.readPoliciesAt(ctx, d)
	if err != nil {
		return err
	}
	selectors, err := policySelectors(policies)
	if err != nil {
		return err
	}

	var covered, managed int
	for _, p := range pods {
		if p.hostNetwork {
			// hostNetwork 的 Pod 根本不受 NetworkPolicy 管辖，把它算进覆盖率
			// 的分母会让一个永远封不住的敞口显示成"覆盖率还差一点"。
			q.UnmanagedPodCount++
			continue
		}
		managed++
		if coveredBySelector(selectors[p.namespace], p.labels) {
			covered++
			continue
		}
		q.NakedPodCount++
	}
	q.PolicyCoverage = safeRate(covered, managed)
	return nil
}

// policySelectors 把落库的策略原文解析成按命名空间分组的 podSelector。
//
// 命名空间取自表列而不是 manifest 里的 metadata：那一列是采集当时写下的
// 事实，而 manifest 是原文证据，两者不一致时该信前者。
//
// 解析不了的原文整体报错，**不跳过**：一条读不懂的策略被跳过之后，它覆盖的
// Pod 会被算成裸 Pod，于是一次解析失败显示成一批没有策略保护的工作负载 ——
// 一个关于集群的结论，而事实只是我们没读懂那份原文。
func policySelectors(policies []observedPolicy) (map[string][]labels.Selector, error) {
	out := map[string][]labels.Selector{}
	for _, p := range policies {
		var np networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(p.manifest), &np); err != nil {
			return nil, fmt.Errorf(
				"collectstore: a stored network policy in namespace %s cannot be parsed: %w", p.namespace, err)
		}
		sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
		if err != nil {
			return nil, fmt.Errorf(
				"collectstore: a stored network policy in namespace %s carries an unusable pod selector: %w",
				p.namespace, err)
		}
		out[p.namespace] = append(out[p.namespace], sel)
	}
	return out, nil
}

// coveredBySelector 报告一组标签是否被该命名空间下任意一条策略选中。
func coveredBySelector(selectors []labels.Selector, podLabels map[string]string) bool {
	set := labels.Set(podLabels)
	for _, sel := range selectors {
		if sel.Matches(set) {
			return true
		}
	}
	return false
}

// fleetRegistry 按当前注册表现装一份网段登记，用来认出跨集群的对端。
//
// 每个请求现装而不是启动时装一次：网段登记从后台改，改完必须立即生效 ——
// 与 Reader 持有注册表读取面而不是集群快照同一条理由。
//
// 网段解析不了的集群照样进登记表，只是没有网段：让它整个消失会让
// cluster.Classify 把它的 Pod IP 判成 EXTERNAL，而带着空网段留下来只会让
// 判定退化成 UNKNOWN。**失败方向朝"答不出"，不朝"自信地答错"。**
func (r *Reader) fleetRegistry(ctx context.Context) (*cluster.Registry, error) {
	list, err := r.src.Clusters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]cluster.Cluster, 0, len(list))
	for _, c := range list {
		out = append(out, cluster.Cluster{
			ID:        c.ID,
			PodCIDRs:  singlePrefix(c.PodCIDR),
			NodeCIDRs: singlePrefix(c.NodeCIDR),
		})
	}
	return cluster.NewRegistry(out), nil
}

// singlePrefix 把一个 CIDR 字符串包成单元素切片；解析不了时返回空。
//
// registry.Cluster 的网段字段目前是单个 string，也没有 Service 网段字段
// （多值化是一次独立的迁移）。于是 Service ClusterIP 会被判成 UNKNOWN 而
// 不是 EXTERNAL —— 那是 internal/cluster 那条"没登记就不许答 EXTERNAL"的
// 直接后果，不是这里漏了什么。
func singlePrefix(cidr string) []netip.Prefix {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil
	}
	return []netip.Prefix{p}
}

// crossesCluster 报告一个地址是否落在**另一个**已登记集群的网段里。
//
// 只认明确命中另一个集群的那一种。AMBIGUOUS 与 UNKNOWN 都不算：前者是网段
// 登记重叠，后者是登记有缺口，两者都不是"这条连接跨了集群"这个结论 ——
// 它们各自会以 UNKNOWN 的形式出现在别处。地址解析不了同样不算。
func crossesCluster(fleet *cluster.Registry, clusterID, ip string) bool {
	if ip == "" {
		return false
	}
	c, err := fleet.Classify(ip)
	if err != nil {
		return false
	}
	switch c.Scope {
	case cluster.ScopePod, cluster.ScopeService, cluster.ScopeNode:
		return c.ClusterID != "" && c.ClusterID != clusterID
	case cluster.ScopeExternal, cluster.ScopeUnknown, cluster.ScopeAmbiguous:
		return false
	default:
		return false
	}
}

// safeRate 算比例，分母为 0 时返回 0 而不是 NaN。
//
// NaN 序列化成 JSON 会失败，而一个失败的 /quality 请求会被读成"这个集群
// 有问题"，事实只是它这个窗口里没有连接。
func safeRate(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}
