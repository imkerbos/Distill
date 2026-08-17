package collectstore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
	"github.com/imkerbos/Distill/internal/store"
)

// maxSecurityFindings 是这份报告里任意一个清单允许装下的条数上限。
//
// 三个清单此前一条上限都没有：上界只来自事实层的 maxWindowConnections，
// 于是一个把数据库端口用满的集群能让一次 /security 在内存里组出几万条
// RiskyFlow（每条含完整 FlowRecord）并整份序列化。调用方选不了这个规模，
// 页面刷新频率乘以单次代价正是 design doc §9 点名的那个失控方向
// （安全规范 §23 / §24）。
//
// 取值与 Flows 那道 maxFlowLimit 一致：同一个平台上"一屏能装多少"只该有
// 一个量级。
const maxSecurityFindings = 1000

// ErrTooManyFindings 表示这份报告装不下这么多发现。
//
// **超出即拒绝，不截断** —— 与事实层的 maxWindowConnections、本包的
// maxIntervalRows 同一条理由，而在这里更硬：一份被悄悄截到 1000 条的风险
// 清单读起来就是"这个集群只有这些风险"，而这正是这个平台唯一不能出的那种
// 错。SecurityReport 上没有回显截断的字段（那是 store 契约的事，两个 Reader
// 与前端要一起改），因此现在唯一"可见"的截断就是不截断。
//
// **这个取舍的代价与解决方向记在 design doc §10**，包括裸奔清单那一道为什么
// 缩小窗口也绕不开、以及接 httpapi 时这个哨兵必须映射成说得出原因的响应。
var ErrTooManyFindings = errors.New("collectstore: this security report holds more findings than it can show")

// tooManyFindings 拼出一句说得出"多的是哪一类、该怎么办"的拒绝。
//
// 不含 SQL、路径与内部地址（安全规范 §22）：这句话会被渲染给操作者，
// 而它要回答的是"我该做什么"，不是"这个查询长什么样"。
func tooManyFindings(clusterID, what, advice string) error {
	return fmt.Errorf("%w: cluster %s holds more than %d %s; %s",
		ErrTooManyFindings, clusterID, maxSecurityFindings, what, advice)
}

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
			if len(rep.RiskyFlows) > maxSecurityFindings {
				return store.SecurityReport{}, tooManyFindings(
					clusterID, "connections on a risky port", "narrow the window")
			}
		}

		if !t.externalAddress(c.Dest.IP) {
			continue
		}
		target, seen := targets[c.Dest.IP]
		if !seen {
			target = &store.EgressTarget{Address: c.Dest.IP}
			targets[c.Dest.IP] = target
			if len(targets) > maxSecurityFindings {
				return store.SecurityReport{}, tooManyFindings(
					clusterID, "distinct addresses outside every registered range", "narrow the window")
			}
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
		if len(out) > maxSecurityFindings {
			return nil, tooManyFindings(t.clusterID, "pods not selected by any policy",
				"the coverage figure on the quality page still answers how bad it is")
		}
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
