package collectstore

import (
	"context"
	"sort"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
	"github.com/imkerbos/Distill/internal/store"
)

// 三个清单的条数上限是 store.MaxSecurityFindings，截断由 store.TruncateFindings
// 完成，截断前的条数与生效上限随报告回显（design doc 2026-08-17 §3）。
//
// 上限本身是必需的：三个清单的上界原本只来自事实层的 maxWindowConnections，
// 一个把数据库端口用满的集群能让一次 /security 整份序列化几万条 RiskyFlow
// （每条含完整 FlowRecord），而调用方选不了这个规模（安全规范 §23 / §24）。
//
// **这里此前是超出即拒绝**，理由是报告上没有任何字段说得出自己被截过 ——
// 一份被悄悄截到 1000 条的风险清单读起来就是"这个集群只有这些风险"。
// 回显字段落地之后那条理由消失了，而拒绝的代价还在：一个 4000 Pod、覆盖率
// 60% 的健康集群会被整份拒绝，且 NakedPods 来自锚点快照，缩小窗口救不了
// （real-reader design §10 记的那笔账）。因此现在截断、并把截断说出来。

// Security 汇总一个集群在指定窗口里的安全发现。
//
// 风险分类回答的是"这条连接值不值得有人去看"，与"这条连接通不通"是两个
// 问题：一次被 DENY 的数据库直连仍然意味着有人在尝试连数据库。因此判不出
// 结论的连接照样进风险清单 —— 归属不明恰恰是更该被看见的那一类。
//
// 裸奔 Pod 来自资产快照而非流量，不受本窗口约束（store.SecurityReport.Window
// 的说明）：两类数据时间语义不同，混在一起会被读成"这段时间内有 N 个裸奔
// Pod"。
func (r *Reader) Security(
	ctx context.Context, clusterID string, window store.TimeWindow,
) (store.SecurityReport, error) {
	if !window.Valid() {
		return store.SecurityReport{}, store.ErrWindowRequired
	}
	t, err := r.readTraffic(ctx, clusterID, flow.Window{From: window.From, To: window.To})
	if err != nil {
		return store.SecurityReport{}, err
	}

	rep := store.SecurityReport{
		Cluster:         clusterID,
		Window:          window,
		RiskyFlows:      []store.RiskyFlow{},
		EgressTargets:   []store.EgressTarget{},
		NakedPods:       []store.NakedPod{},
		RiskPortCatalog: store.RiskPortCatalog(),
	}

	targets := map[string]*store.EgressTarget{}
	for i, c := range t.conns {
		a := t.attribute(c)
		rec := t.record(i, a)

		if rp, risky := risk.Lookup(c.Port); risky {
			rep.RiskyFlows = append(rep.RiskyFlows, store.RiskyFlow{
				FlowRecord: rec,
				Category:   rp.Category,
				Position:   t.riskPosition(a),
				PortName:   rp.Name,
			})
		}

		if !t.externalAddress(c.Dest.IP) {
			continue
		}
		target, seen := targets[c.Dest.IP]
		if !seen {
			target = &store.EgressTarget{Address: c.Dest.IP}
			targets[c.Dest.IP] = target
		}
		target.FlowCount++
		switch rec.Verdict {
		case string(replay.VerdictAllow):
			target.AllowedCount++
		case string(replay.VerdictUnknown):
			target.UnknownCount++
		}
		if !containsPort(target.Ports, c.Port) {
			target.Ports = append(target.Ports, c.Port)
		}
	}

	for _, target := range targets {
		sort.Slice(target.Ports, func(i, j int) bool { return target.Ports[i] < target.Ports[j] })
		rep.EgressTargets = append(rep.EgressTargets, *target)
	}
	sort.Slice(rep.EgressTargets, func(i, j int) bool {
		return rep.EgressTargets[i].Address < rep.EgressTargets[j].Address
	})
	sort.Slice(rep.RiskyFlows, func(i, j int) bool {
		return rep.RiskyFlows[i].ID < rep.RiskyFlows[j].ID
	})

	naked, err := t.nakedPods()
	if err != nil {
		return store.SecurityReport{}, err
	}
	rep.NakedPods = naked

	// 三个清单都在排序之后才截：留下哪一批取决于内容，不取决于事实层的
	// 读取顺序，否则同一次查询在两次刷新之间会换一批发现，而调用方无从察觉。
	rep.RiskyFlows, rep.RiskyFlowsTruncation = store.TruncateFindings(rep.RiskyFlows)
	rep.EgressTargets, rep.EgressTargetsTruncation = store.TruncateFindings(rep.EgressTargets)
	rep.NakedPods, rep.NakedPodsTruncation = store.TruncateFindings(rep.NakedPods)
	return rep, nil
}

// riskPosition 判断一条风险流量所处的位置。
//
// 两端都要解得出主体才谈得上"同 namespace"。归属不明时保守归入跨
// namespace：把一条来源不明的连接说成内部正常调用，是这份报告最不该犯的
// 错 —— 它会让唯一一条值得看的记录沉到底。
func (t traffic) riskPosition(a attributed) store.RiskPosition {
	if t.externalAddress(a.conn.Dest.IP) {
		return store.PositionEgressInternet
	}
	if a.srcOutcome == identity.OutcomeResolved && a.dstOutcome == identity.OutcomeResolved &&
		a.src.Namespace == a.dst.Namespace {
		return store.PositionSameNamespace
	}
	return store.PositionCrossNamespace
}

// nakedPods 点名锚点那一刻没有被任何 NetworkPolicy 选中的 Pod。
//
// hostNetwork 的 Pod 不计入：它们不是"没被策略选中"，而是 NetworkPolicy 根本
// 管不到，混在一起会让两类完全不同的敞口显示成同一件事，而处置手段不同 ——
// 前者补一条策略，后者只能靠别的手段。口径与 Quality.NakedPodCount 完全一致，
// 两处给出不同的数会让运维照着一份自己对不上的账去排查。
func (t traffic) nakedPods() ([]store.NakedPod, error) {
	selectors, err := selectorsByNamespace(t.policies)
	if err != nil {
		return nil, err
	}

	out := []store.NakedPod{}
	for _, p := range t.pods {
		if p.hostNetwork || coveredBySelector(selectors[p.namespace], p.labels) {
			continue
		}
		out = append(out, store.NakedPod{Cluster: t.clusterID, Namespace: p.namespace, Name: p.name})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// containsPort 报告端口是否已经记过。
func containsPort(ports []int32, p int32) bool {
	for _, x := range ports {
		if x == p {
			return true
		}
	}
	return false
}
