package collectstore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/store"
)

// unknownWorkload 是工作负载归属解不出来时的节点名。
//
// 与 fixture 侧同一个取值：归属不明的 Pod 照常成节点，把它们藏起来会让
// workload 拓扑看上去比实际完整，而那个缺口恰恰是该显示出来的东西。
const unknownWorkload = "UNKNOWN"

// Topology 返回一个集群在最近一次观测窗口里的通信拓扑。
//
// 没有可用采集时返回 ErrNoCollection，**不是一份空拓扑**：空拓扑读起来是
// "这个集群里什么都没有"，那是一句没有人确认过的话（design doc §2）。
//
// 边上的 verdict 一律是 UNKNOWN：本轮**没有做任何策略回放**，而"策略放行了
// 它"这句话只能由回放引擎说（design doc §7 把 Flows 与 PolicyPreview 排在
// 后面）。把来源报告的 ALLOWED 顶上去会让"现网放行"读成"策略允许"，那是两
// 件事；填成 ALLOW 更是把一个没做过的判定说成做过了。confidence 则如实
// 传导窗口完整度（§4）。
func (r *Reader) Topology(
	ctx context.Context, clusterID string, level store.TopologyLevel,
) (store.Topology, error) {
	if !store.ValidTopologyLevel(string(level)) {
		// 拒绝而不是静默退回 namespace 粒度：界面会展示 namespace 粒度却
		// 以为自己在看 workload 粒度。
		return store.Topology{}, fmt.Errorf("collectstore: unregistered topology level %q", string(level))
	}
	// 先按有流量的路子来；没有摄入过就退回只用资产作答
	// （design doc 2026-08-18 §4.1）。**不是降级**：节点本来就只依赖身份区间
	// 与锚点快照的策略，边才需要观测。
	trafficObserved := true
	d, err := r.describe(ctx, clusterID)
	if errors.Is(err, ErrNoFlowIngest) {
		trafficObserved = false
		d, err = r.describeAssets(ctx, clusterID)
	}
	if err != nil {
		return store.Topology{}, err
	}

	policyNamespaces, err := r.policyNamespaces(ctx, d)
	if err != nil {
		return store.Topology{}, err
	}

	nodes := d.nodes(level, policyNamespaces)
	edges, unplaceable := d.edges(level)
	return store.Topology{
		Nodes:                nodes,
		Edges:                edges,
		UnplaceableFlowCount: unplaceable,
		Level:                string(level),
		TrafficObserved:      trafficObserved,
	}, nil
}

// policyNamespaces 返回锚点那一刻存在 NetworkPolicy 的命名空间集合。
func (r *Reader) policyNamespaces(ctx context.Context, d described) (map[string]bool, error) {
	policies, err := r.readPoliciesAt(ctx, d)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(policies))
	for _, p := range policies {
		out[p.namespace] = true
	}
	return out, nil
}

// nodeAccumulator 累积同一个节点下多个 Pod 的计数。
//
// 按 Pod UID 去重：同一个节点上的多个 hostNetwork Pod 共用节点地址，
// 它们在区间表里是同一个地址下的多条记录，按地址计数会把它们合成一个。
type nodeAccumulator struct {
	node store.TopologyNode
	pods map[string]bool
}

// nodes 用覆盖那一刻的区间构造节点。
//
// 节点来自**区间表**而不是最新一次快照（design doc §5）：地址会被回收，
// 拿今天的快照描述一段过去的观测，会把那时的流量画到今天的工作负载上。
// 边的归属同样出自这批区间，两者因此永远说的是同一批主体。
func (d described) nodes(level store.TopologyLevel, hasPolicy map[string]bool) []store.TopologyNode {
	acc := map[string]*nodeAccumulator{}
	for _, iv := range d.living {
		id, label := nodeIDOf(d.clusterID, iv.Identity, level)
		n, ok := acc[id]
		if !ok {
			n = &nodeAccumulator{
				node: store.TopologyNode{
					ID:        id,
					Cluster:   d.clusterID,
					Namespace: label,
					HasPolicy: hasPolicy[iv.Identity.Namespace],
				},
				pods: map[string]bool{},
			}
			acc[id] = n
		}
		if n.pods[iv.Identity.PodUID] {
			continue
		}
		n.pods[iv.Identity.PodUID] = true
		n.node.PodCount++
		if iv.Identity.HostNetwork {
			n.node.UnmanagedPodCount++
		}
		if iv.Identity.InMesh {
			n.node.InMesh = true
		}
	}

	out := make([]store.TopologyNode, 0, len(acc))
	for _, n := range acc {
		out = append(out, n.node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// nodeIDOf 按粒度给一个主体算出节点标识与展示名。
func nodeIDOf(clusterID string, id identity.Identity, level store.TopologyLevel) (nodeID, label string) {
	if level == store.LevelWorkload {
		wl := id.WorkloadName
		if wl == "" {
			wl = unknownWorkload
		}
		return clusterID + "/" + id.Namespace + "/" + wl, id.Namespace + "/" + wl
	}
	return clusterID + "/" + id.Namespace, id.Namespace
}

// edgeKey 是拓扑边的聚合键。
type edgeKey struct {
	source string
	target string
}

// edgeAccumulator 累积同一对节点之间多条连接的聚合结果。
type edgeAccumulator struct {
	flowCount int
	unmanaged bool
	ports     map[int32]struct{}
}

// edges 把连接按节点对聚合成边，并报出没能落到任何一条边上的连接条数。
//
// 两端都要解得出主体才画边。解不出来的**不丢弃**（design doc §3）：它们进
// UnplaceableFlowCount，由调用方一并报出。丢掉会让观测到的连接集合小于真实
// 集合，于是覆盖它们的规则被判"无流量、可收紧"—— 那个单向的失败方向。
//
// 因此本轮不会出现跨集群的边，边上的 CrossCluster 一律是 false：对端的
// 命名空间要读对面集群的区间，而这里只解本集群那一份。跨集群的那些连接
// 于是落进 UnplaceableFlowCount，并在 Quality 里由 CrossClusterCount 点名 ——
// 它们没有消失，只是还画不出来。
func (d described) edges(level store.TopologyLevel) ([]store.TopologyEdge, int) {
	acc := map[edgeKey]*edgeAccumulator{}
	unplaceable := 0

	for _, c := range d.conns {
		src, srcOutcome := d.subjectAt(c.Source)
		dst, dstOutcome := d.subjectAt(c.Dest)
		// HOST_NETWORK 分两种：那个节点上只跑着一个 hostNetwork Pod 时身份是
		// 解出来的，画得上图；多个共用节点 IP 时 Identity 是零值，那时才真的
		// 定位不了。按 Outcome 一刀切会把前者也算进「无法定位」，而它明明有
		// 名有姓。
		placeable := func(o identity.Outcome, id identity.Identity) bool {
			switch o {
			case identity.OutcomeResolved:
				return true
			case identity.OutcomeHostNetwork:
				return id.PodName != ""
			default:
				return false
			}
		}
		if !placeable(srcOutcome, src) || !placeable(dstOutcome, dst) {
			unplaceable++
			continue
		}
		srcID, _ := nodeIDOf(d.clusterID, src, level)
		dstID, _ := nodeIDOf(d.clusterID, dst, level)

		k := edgeKey{source: srcID, target: dstID}
		a, ok := acc[k]
		if !ok {
			a = &edgeAccumulator{ports: map[int32]struct{}{}}
			acc[k] = a
		}
		a.flowCount++
		a.ports[c.Port] = struct{}{}
		// hostNetwork 的一端不受 NetworkPolicy 管辖。缺了这个标记，一条通往
		// 特权组件的边会渲染成普通放行，与"策略确实允许了它"无法区分。
		a.unmanaged = a.unmanaged || src.HostNetwork || dst.HostNetwork
	}

	out := make([]store.TopologyEdge, 0, len(acc))
	for k, a := range acc {
		ports := make([]int32, 0, len(a.ports))
		for p := range a.ports {
			ports = append(ports, p)
		}
		sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
		out = append(out, store.TopologyEdge{
			Source:     k.source,
			Target:     k.target,
			Verdict:    string(replay.VerdictUnknown),
			Confidence: string(confidenceOf(d.completeness)),
			FlowCount:  a.flowCount,
			Ports:      ports,
			Unmanaged:  a.unmanaged,
			// DecidedBy 留空：做出判定的方向只有回放引擎说得出，而本轮
			// 一次判定都没做。填一个方向等于给"该改哪边的策略"一个
			// 凭空的答案。
			DecidedBy: "",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Target < out[j].Target
	})
	return out, unplaceable
}
