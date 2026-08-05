package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/replay"
)

// defaultFlowLimit 是流量列表未指定 Limit 时的返回条数上限。
//
// 取值必须大于整个数据集（spec §3.4：规模刻意小到能实时求值），否则默认
// 视图会沿 ID 序截断，而 UNKNOWN 与跨集群流量恰好排在序列后段 —— 一个把
// 敞口切掉的默认值比没有默认值更糟。
const defaultFlowLimit = 500

// unspecifiedUnknownReason 是 UNKNOWN 但未给出原因时在构成明细里的归类。
//
// 不能因为没有原因就不计入：明细各项之和必须等于 UNKNOWN 总数，
// 否则运维会照着一份自己少加了一项的账去排查。
const unspecifiedUnknownReason = "UNSPECIFIED"

// ErrClusterNotFound 表示请求的集群不存在。
var ErrClusterNotFound = errors.New("cluster not found")

// ErrWindowRequired 表示查询缺少必填的时间窗。
var ErrWindowRequired = errors.New("time window required")

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
// 没有集群归属（如出公网）时退回源端。策略在目的端生效，所以"归谁判"
// 只能有这一个答案；Clusters 的流量计数与 Flows 的集群筛选也沿用它，
// 保证同一条 flow 不会在两个集群下各算一次。
//
// 注意它回答的不是"这条 flow 与谁有关"——跨集群流量与两端都有关，
// 那个问题由 involvesCluster 回答。
func owningCluster(f replay.Flow) string {
	if f.Dest.ClusterID != "" {
		return f.Dest.ClusterID
	}
	return f.Source.ClusterID
}

// involvesCluster 判断该 flow 是否与指定集群有关：任一端属于它即算。
//
// 拓扑与数据质量用这条规则而非 owningCluster：跨集群流量只归给目的端集群
// 的话，源端集群的界面会报出 crossClusterCount: 0 —— 那不是"没有跨集群
// 流量"，而是一句斩钉截铁的假话，它自己的 namespace 正在往外集群发流量。
// 判定仍然由目的端集群的求值器做出（策略在目的端生效），这两件事不冲突。
func involvesCluster(f replay.Flow, clusterID string) bool {
	return f.Source.ClusterID == clusterID || f.Dest.ClusterID == clusterID
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
		Timestamp:     f.Flow.Timestamp,
		SourceLabel:   endpointLabel(f.Flow.Source),
		DestLabel:     endpointLabel(f.Flow.Dest),
		Protocol:      string(f.Flow.Protocol),
		Port:          f.Flow.Port,
		Verdict:       string(d.Verdict),
		Confidence:    string(d.Confidence),
		UnknownReason: string(d.UnknownReason),
		CrossCluster:  d.CrossCluster,
		Unmanaged:     d.Reason.Unmanaged,
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

// verdictSeverity 给结论排定严重程度，用于多条 flow 聚合成一条边时取最
// 严重者：UNKNOWN 未知优先于 DENY 已知阻断，两者都优先于 ALLOW——一条边
// 下只要有一条 flow 不是单纯放行，就不能让边整体显示 ALLOW。
//
// 未登记的 verdict 排在最严重一档，而不是落到 0：新增一种结论却漏改这里
// 时，它会被顶到边上显眼处，而不是永远输给 ALLOW、悄悄消失在一片绿里。
func verdictSeverity(v string) int {
	switch replay.Verdict(v) {
	case replay.VerdictAllow:
		return 1
	case replay.VerdictDeny:
		return 2
	case replay.VerdictUnknown:
		return 3
	default:
		return 4
	}
}

// mergeVerdict 在聚合同一条边的多条 flow 时取更严重的 verdict。
func mergeVerdict(cur, next string) string {
	if cur == "" || verdictSeverity(next) > verdictSeverity(cur) {
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
	unmanaged    bool
	flowCount    int
	ports        map[int32]struct{}
}

// recordForeignNode 在端点所在命名空间不属于本次查询集群的已知节点集合时，
// 记一个 Foreign 占位节点。跨集群边如果只画边、不画对端节点，前端图会拿到
// 一条指向不存在节点的悬空引用——渲染器要么报错，要么把这条边悄悄丢掉，
// 而丢掉的恰恰是"跨集群没有策略管控"这个平台最该展示出来的敞口。
// pod 计数与 HasPolicy 留空：本集群的快照里确实不认识对面集群的资产，
// 编一个数字比坦白"不知道"更危险。
func recordForeignNode(foreign map[string]TopologyNode, known map[string]bool, ep replay.Endpoint) {
	if ep.Pod == nil {
		return
	}
	id := ep.Pod.ClusterID + "/" + ep.Pod.Namespace
	if known[id] {
		return
	}
	if _, seen := foreign[id]; seen {
		return
	}
	foreign[id] = TopologyNode{
		ID:        id,
		Cluster:   ep.Pod.ClusterID,
		Namespace: ep.Pod.Namespace,
		Foreign:   true,
	}
}

// topologyEdges 把该集群相关的 flow 按 (源命名空间, 目的命名空间) 聚合成边，
// 同时收集边所引用、但不属于 known（本集群自有命名空间）的对端节点，
// 以及因端点无法定位而没能落到任何一条边上的流量条数。
func (r *FixtureReader) topologyEdges(clusterID string, known map[string]bool) ([]TopologyEdge, []TopologyNode, int) {
	acc := make(map[edgeKey]*edgeAccumulator)
	foreign := make(map[string]TopologyNode)
	var unplaceable int
	for _, f := range r.fleet.Flows {
		if !involvesCluster(f.Flow, clusterID) {
			continue
		}
		srcID, srcOK := nsNodeID(f.Flow.Source)
		dstID, dstOK := nsNodeID(f.Flow.Dest)
		if !srcOK || !dstOK {
			// 画不出这条边，但它确实存在。计数后由 Topology 一并报出，
			// 否则拓扑页的流量总和会比数据质量页少一截且无人解释。
			unplaceable++
			continue
		}
		recordForeignNode(foreign, known, f.Flow.Source)
		recordForeignNode(foreign, known, f.Flow.Dest)

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
		a.unmanaged = a.unmanaged || d.Reason.Unmanaged
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
			Unmanaged:    a.unmanaged,
		})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Source != edges[j].Source {
			return edges[i].Source < edges[j].Source
		}
		return edges[i].Target < edges[j].Target
	})

	foreignNodes := make([]TopologyNode, 0, len(foreign))
	for _, n := range foreign {
		foreignNodes = append(foreignNodes, n)
	}
	return edges, foreignNodes, unplaceable
}

// Topology 返回指定集群的通信拓扑。集群不存在时返回错误。
func (r *FixtureReader) Topology(_ context.Context, clusterID string) (Topology, error) {
	c, ok := r.fleet.Cluster(clusterID)
	if !ok {
		return Topology{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}

	nodes := topologyNodes(c)
	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
	}

	edges, foreignNodes, unplaceable := r.topologyEdges(c.ID, known)
	nodes = append(nodes, foreignNodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	return Topology{Nodes: nodes, Edges: edges, UnplaceableFlowCount: unplaceable}, nil
}

// DataWindow 返回覆盖本数据集全部流量的时间窗。
//
// 只在 fixture 实现上提供，不进 Reader 接口：真实存储的数据窗口是无界的，
// "全部数据的时间范围"在那里不是一个有意义的问题。
//
// 存在的理由是给装配方一个不会过期的默认窗口。fixture 数据固定在 baseTime，
// 任何"最近 N 天"的默认值都会随真实时间推移而在某天悄悄返回 0 条 —— demo
// 会在没有人改动代码的情况下自己坏掉。
//
// 上界取末条 flow 之后一秒：TimeWindow 左闭右开，用末条时间戳本身会把
// 最后一条排除在外。
func (r *FixtureReader) DataWindow() TimeWindow {
	if len(r.fleet.Flows) == 0 {
		// 空数据集没有有意义的窗口，但返回零值会让调用方拿到一个
		// Valid() 为假的窗口并在查询时才失败。给一个退化但合法的区间。
		now := time.Time{}
		return TimeWindow{From: now, To: now.Add(time.Second)}
	}
	first := r.fleet.Flows[0].Flow.Timestamp
	last := first
	for _, fl := range r.fleet.Flows {
		if fl.Flow.Timestamp.Before(first) {
			first = fl.Flow.Timestamp
		}
		if fl.Flow.Timestamp.After(last) {
			last = fl.Flow.Timestamp
		}
	}
	return TimeWindow{From: first, To: last.Add(time.Second)}
}

// Flows 按条件返回流量列表。筛选条件指向不存在的集群时返回错误。
func (r *FixtureReader) Flows(_ context.Context, filter FlowFilter) (FlowPage, error) {
	// 先于集群校验：缺时间窗是调用方用错了接口，与查哪个集群无关。
	if !filter.Window.Valid() {
		return FlowPage{}, ErrWindowRequired
	}
	if filter.Cluster != "" {
		if _, ok := r.fleet.Cluster(filter.Cluster); !ok {
			// 与 Topology、Quality 一致地把"集群不存在"报成错误：同一个
			// 条件在一个接口上返回空列表、在另一个接口上返回 20002，
			// 会让前端把打错的集群名读成"这个集群没有流量"。
			return FlowPage{}, fmt.Errorf("%w: %s", ErrClusterNotFound, filter.Cluster)
		}
	}

	limit := filter.Limit
	if limit == 0 {
		limit = defaultFlowLimit
	}

	out := make([]FlowRecord, 0, len(r.fleet.Flows))
	for _, f := range r.fleet.Flows {
		// involvesCluster，不是 owningCluster：这个筛选器是从 Topology/Quality
		// 点进来的钻取入口，要用它们同一条"是否算这个集群的"规则，否则跨集群
		// flow 在概览页被计入源集群，点进列表却因为目的端不是它而被过滤掉。
		if !filter.Window.Contains(f.Flow.Timestamp) {
			continue
		}
		if filter.Cluster != "" && !involvesCluster(f.Flow, filter.Cluster) {
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
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return FlowPage{Items: out, Total: total, Limit: limit, Window: filter.Window}, nil
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
	var trusted, degraded int
	for _, f := range r.fleet.Flows {
		if !involvesCluster(f.Flow, clusterID) {
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
			q.UnknownCount++
			if d.UnknownReason != replay.ReasonNone {
				q.UnknownComposition[string(d.UnknownReason)]++
			} else {
				q.UnknownComposition[unspecifiedUnknownReason]++
			}
		}
		if d.CrossCluster {
			q.CrossClusterCount++
		}
	}
	q.TrustedRate = safeRate(trusted, q.TotalFlows)
	q.UnknownRate = safeRate(q.UnknownCount, q.TotalFlows)
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
