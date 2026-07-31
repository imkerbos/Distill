package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/replay"
)

// defaultFlowLimit 是流量列表未指定 Limit 时的返回条数上限。
const defaultFlowLimit = 100

// ErrClusterNotFound 表示请求的集群不存在。
var ErrClusterNotFound = errors.New("cluster not found")

// FixtureReader 用合成数据实现 Reader。
//
// 判定在查询时现场求值而非预计算：数据量小，性能无压力，
// 而现场求值让判定解释器真正展示引擎在工作，而不是查一张表。
type FixtureReader struct {
	fleet      fixture.Fleet
	evaluators map[string]*replay.Evaluator
}

// NewFixtureReader 用合成数据构造查询器。
func NewFixtureReader(f fixture.Fleet) *FixtureReader {
	evals := make(map[string]*replay.Evaluator, len(f.Clusters))
	for _, c := range f.Clusters {
		var opts []replay.Option
		if c.CCNPPresent {
			opts = append(opts, replay.WithCCNPPresent(true))
		}
		evals[c.ID] = replay.NewEvaluator(c.ID, c.Policies, c.Namespaces, opts...)
	}
	return &FixtureReader{fleet: f, evaluators: evals}
}

// owningCluster 返回负责对该 flow 求值的集群：优先取目的端集群，目的端
// 没有集群归属（如出公网）时退回源端。Clusters、Flows、Topology、Quality
// 统一用这一条规则判断"这条 flow 算谁的"，避免同一份数据在不同接口上
// 因为口径不一致而报出互相矛盾的计数。
func owningCluster(f replay.Flow) string {
	if f.Dest.ClusterID != "" {
		return f.Dest.ClusterID
	}
	return f.Source.ClusterID
}

// decide 对一条 flow 求值。
//
// 用目的端所属集群的求值器：NetworkPolicy 是集群本地对象，
// 判定必须由承载目的 Pod 的那个集群的策略集做出。
func (r *FixtureReader) decide(f fixture.Flow) replay.Decision {
	clusterID := owningCluster(f.Flow)
	e, ok := r.evaluators[clusterID]
	if !ok {
		// 两端都不属于任何已知集群，无从判定。
		return replay.Decision{Verdict: replay.VerdictUnknown,
			Confidence: replay.ConfidenceTrusted, UnknownReason: replay.ReasonExternalNoIdentity}
	}
	return e.Evaluate(f.Flow)
}

// endpointLabel 返回端点的可读标签，供列表展示。
func endpointLabel(ep replay.Endpoint) string {
	if ep.Pod != nil {
		return fmt.Sprintf("%s/%s/%s", ep.Pod.ClusterID, ep.Pod.Namespace, ep.Pod.Name)
	}
	if ep.ClusterID != "" {
		return fmt.Sprintf("%s/? (%s)", ep.ClusterID, ep.IP)
	}
	return ep.IP
}

// toFlowRecord 把一条 flow 求值并映射成 API 形状，供 Flows 与 Flow 共用，
// 避免字段列表在两处各写一份、改一处漏改另一处。
func (r *FixtureReader) toFlowRecord(f fixture.Flow) (FlowRecord, replay.Decision) {
	d := r.decide(f)
	rec := FlowRecord{
		ID:            f.ID,
		SourceLabel:   endpointLabel(f.Flow.Source),
		DestLabel:     endpointLabel(f.Flow.Dest),
		Protocol:      string(f.Flow.Protocol),
		Port:          f.Flow.Port,
		Verdict:       string(d.Verdict),
		Confidence:    string(d.Confidence),
		UnknownReason: string(d.UnknownReason),
		CrossCluster:  d.CrossCluster,
	}
	return rec, d
}

// safeRate 把计数换算成 [0,1] 的比例，分母为 0 时取 0 而不是 NaN——
// 数据质量页面存在的意义就是如实说明"不知道"，NaN 序列化成 JSON
// 要么报错要么变成 null，两种都是这个页面唯一不能做的事：说谎。
func safeRate(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

// Clusters 返回全部集群概览。
func (r *FixtureReader) Clusters(_ context.Context) ([]ClusterSummary, error) {
	out := make([]ClusterSummary, 0, len(r.fleet.Clusters))
	for _, c := range r.fleet.Clusters {
		var flowCount int
		for _, f := range r.fleet.Flows {
			if owningCluster(f.Flow) == c.ID {
				flowCount++
			}
		}
		out = append(out, ClusterSummary{
			ID:             c.ID,
			NamespaceCount: len(c.Namespaces),
			PodCount:       len(c.Pods),
			FlowCount:      flowCount,
			CCNPPresent:    c.CCNPPresent,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// podsInNamespace 从集群快照里筛出指定命名空间的 Pod。
func podsInNamespace(pods []replay.PodRef, ns string) []replay.PodRef {
	var out []replay.PodRef
	for _, p := range pods {
		if p.Namespace == ns {
			out = append(out, p)
		}
	}
	return out
}

// namespaceInMesh 判断命名空间是否处于 mesh 中。
//
// 两个信号都要看：istio-injection=enabled 是 Istio 自动注入 sidecar 的
// 标准命名空间标签，命名空间创建时就能看到；但标签可能滞后于实际状态，
// 所以再看 Pod 自身的 InMesh（sidecar 是否真的被注入）兜底，两者任一
// 为真即认为该命名空间的 L4 身份不可信。
func namespaceInMesh(ns replay.NamespaceRef, pods []replay.PodRef) bool {
	if ns.Labels["istio-injection"] == "enabled" {
		return true
	}
	for _, p := range pods {
		if p.InMesh {
			return true
		}
	}
	return false
}

// namespaceHasPolicy 判断命名空间下是否存在 NetworkPolicy。
func namespaceHasPolicy(policies []networkingv1.NetworkPolicy, ns string) bool {
	for _, p := range policies {
		if p.Namespace == ns {
			return true
		}
	}
	return false
}

// topologyNodes 构造集群内每个命名空间对应的节点。
func topologyNodes(c fixture.Cluster) []TopologyNode {
	nodes := make([]TopologyNode, 0, len(c.Namespaces))
	for _, ns := range c.Namespaces {
		pods := podsInNamespace(c.Pods, ns.Name)
		node := TopologyNode{
			ID:        c.ID + "/" + ns.Name,
			Cluster:   c.ID,
			Namespace: ns.Name,
			InMesh:    namespaceInMesh(ns, pods),
			HasPolicy: namespaceHasPolicy(c.Policies, ns.Name),
			PodCount:  len(pods),
		}
		for _, p := range pods {
			if p.HostNetwork {
				node.UnmanagedPodCount++
			}
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes
}

// nsNodeID 返回端点所属命名空间对应的节点 ID。身份未还原或外部地址
// 没有命名空间可言，第二个返回值为 false，调用方据此跳过该 flow——
// 拓扑边必须落在具体命名空间之间，无法确定一端就不该画出这条边。
func nsNodeID(ep replay.Endpoint) (string, bool) {
	if ep.Pod == nil {
		return "", false
	}
	return ep.Pod.ClusterID + "/" + ep.Pod.Namespace, true
}

// verdictSeverity 给三种结论排定严重程度，用于多条 flow 聚合成一条边时
// 取最严重者：UNKNOWN 未知优先于 DENY 已知阻断，两者都优先于 ALLOW——
// 一条边下只要有一条 flow 不是单纯放行，就不能让边整体显示 ALLOW。
var verdictSeverity = map[string]int{
	string(replay.VerdictAllow):   1,
	string(replay.VerdictDeny):    2,
	string(replay.VerdictUnknown): 3,
}

// mergeVerdict 在聚合同一条边的多条 flow 时取更严重的 verdict。
func mergeVerdict(cur, next string) string {
	if cur == "" || verdictSeverity[next] > verdictSeverity[cur] {
		return next
	}
	return cur
}

// mergeConfidence 在聚合同一条边的多条 flow 时取更低的 confidence：
// 只要有一条 flow 是 DEGRADED，整条边就不得再显示为 TRUSTED。
func mergeConfidence(cur, next string) string {
	if cur == "" {
		return next
	}
	if cur == string(replay.ConfidenceDegraded) || next == string(replay.ConfidenceDegraded) {
		return string(replay.ConfidenceDegraded)
	}
	return cur
}

// edgeKey 是拓扑边的聚合键：一对命名空间节点之间的所有 flow 合并成一条边。
type edgeKey struct {
	source string
	target string
}

// edgeAccumulator 累积同一对命名空间之间多条 flow 的聚合结果。
type edgeAccumulator struct {
	verdict      string
	confidence   string
	crossCluster bool
	flowCount    int
	ports        map[int32]struct{}
}

// topologyEdges 把该集群相关的 flow 按 (源命名空间, 目的命名空间) 聚合成边。
func (r *FixtureReader) topologyEdges(clusterID string) []TopologyEdge {
	acc := make(map[edgeKey]*edgeAccumulator)
	for _, f := range r.fleet.Flows {
		if owningCluster(f.Flow) != clusterID {
			continue
		}
		srcID, ok := nsNodeID(f.Flow.Source)
		if !ok {
			continue
		}
		dstID, ok := nsNodeID(f.Flow.Dest)
		if !ok {
			continue
		}

		d := r.decide(f)
		k := edgeKey{source: srcID, target: dstID}
		a, ok := acc[k]
		if !ok {
			a = &edgeAccumulator{ports: make(map[int32]struct{})}
			acc[k] = a
		}
		a.verdict = mergeVerdict(a.verdict, string(d.Verdict))
		a.confidence = mergeConfidence(a.confidence, string(d.Confidence))
		a.crossCluster = a.crossCluster || d.CrossCluster
		a.flowCount++
		a.ports[f.Flow.Port] = struct{}{}
	}

	edges := make([]TopologyEdge, 0, len(acc))
	for k, a := range acc {
		ports := make([]int32, 0, len(a.ports))
		for p := range a.ports {
			ports = append(ports, p)
		}
		sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
		edges = append(edges, TopologyEdge{
			Source:       k.source,
			Target:       k.target,
			Verdict:      a.verdict,
			Confidence:   a.confidence,
			CrossCluster: a.crossCluster,
			FlowCount:    a.flowCount,
			Ports:        ports,
		})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Source != edges[j].Source {
			return edges[i].Source < edges[j].Source
		}
		return edges[i].Target < edges[j].Target
	})
	return edges
}

// Topology 返回指定集群的通信拓扑。集群不存在时返回错误。
func (r *FixtureReader) Topology(_ context.Context, clusterID string) (Topology, error) {
	c, ok := r.fleet.Cluster(clusterID)
	if !ok {
		return Topology{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}
	return Topology{
		Nodes: topologyNodes(c),
		Edges: r.topologyEdges(c.ID),
	}, nil
}

// Flows 按条件返回流量列表。
func (r *FixtureReader) Flows(_ context.Context, filter FlowFilter) ([]FlowRecord, error) {
	limit := filter.Limit
	if limit == 0 {
		limit = defaultFlowLimit
	}

	out := make([]FlowRecord, 0, len(r.fleet.Flows))
	for _, f := range r.fleet.Flows {
		if filter.Cluster != "" && owningCluster(f.Flow) != filter.Cluster {
			continue
		}
		rec, _ := r.toFlowRecord(f)
		if filter.Verdict != "" && rec.Verdict != filter.Verdict {
			continue
		}
		if filter.Confidence != "" && rec.Confidence != filter.Confidence {
			continue
		}
		out = append(out, rec)
	}

	// 按 ID 排序保证多次调用顺序稳定；flow-XXXX 是固定宽度的十进制序号，
	// 字符串序与数值序一致。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Flow 返回单条流量的完整判定。不存在时第二个返回值为 false。
func (r *FixtureReader) Flow(_ context.Context, id string) (Decision, bool, error) {
	for _, f := range r.fleet.Flows {
		if f.ID != id {
			continue
		}
		rec, d := r.toFlowRecord(f)
		return Decision{
			FlowRecord: rec,
			Reason: DecisionReason{
				Direction:      string(d.Reason.Direction),
				Isolated:       d.Reason.Isolated,
				Unmanaged:      d.Reason.Unmanaged,
				MatchedPolicy:  d.Reason.MatchedPolicy,
				MatchedRuleIdx: d.Reason.MatchedRuleIdx,
				Detail:         d.Reason.Detail,
			},
		}, true, nil
	}
	return Decision{}, false, nil
}

// podCoveredByPolicy 判断 Pod 是否被某条 NetworkPolicy 的 podSelector 选中。
//
// 只看 podSelector 是否选中，不看策略里的规则能否解析：一条 ipBlock 写错
// 的策略仍然选中了它的目标 Pod、仍然让它们进入隔离状态——覆盖率统计的是
// "被 NetworkPolicy 管到了没有"，不是"策略写对了没有"，两者是不同的问题。
func podCoveredByPolicy(pod replay.PodRef, policies []networkingv1.NetworkPolicy) bool {
	for _, p := range policies {
		if p.Namespace != pod.Namespace {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
		if err != nil {
			// 无法解析的 selector 不能被当成"覆盖了这个 Pod"：覆盖率要如实
			// 反映"确定被管控"，而不是"声称被管控"。
			continue
		}
		if sel.Matches(labels.Set(pod.Labels)) {
			return true
		}
	}
	return false
}

// Quality 返回指定集群的数据质量。集群不存在时返回错误。
func (r *FixtureReader) Quality(_ context.Context, clusterID string) (Quality, error) {
	c, ok := r.fleet.Cluster(clusterID)
	if !ok {
		return Quality{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}

	q := Quality{Cluster: clusterID, UnknownComposition: make(map[string]int)}
	var trusted, unknown, degraded int
	for _, f := range r.fleet.Flows {
		if owningCluster(f.Flow) != clusterID {
			continue
		}
		q.TotalFlows++
		d := r.decide(f)
		switch d.Confidence {
		case replay.ConfidenceTrusted:
			trusted++
		case replay.ConfidenceDegraded:
			degraded++
		}
		if d.Verdict == replay.VerdictUnknown {
			unknown++
			if d.UnknownReason != replay.ReasonNone {
				q.UnknownComposition[string(d.UnknownReason)]++
			}
		}
		if d.CrossCluster {
			q.CrossClusterCount++
		}
	}
	q.TrustedRate = safeRate(trusted, q.TotalFlows)
	q.UnknownRate = safeRate(unknown, q.TotalFlows)
	q.DegradedRate = safeRate(degraded, q.TotalFlows)

	var covered, nonHostNetwork int
	for _, p := range c.Pods {
		if p.HostNetwork {
			q.UnmanagedPodCount++
			continue
		}
		nonHostNetwork++
		if podCoveredByPolicy(p, c.Policies) {
			covered++
		} else {
			q.NakedPodCount++
		}
	}
	q.PolicyCoverage = safeRate(covered, nonHostNetwork)

	return q, nil
}
