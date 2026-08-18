package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
)

// 安全发现的类型与判定依据。
//
// 与判定引擎分开：这里回答的是"这条连接值不值得有人去看"，
// 而不是"这条连接通不通"。后者是 replay 的职责，前者是风险分类，
// 两者混在一起会让"策略挡住了"被读成"没有风险"——一次被 DENY 的
// 数据库直连仍然意味着有人在尝试连数据库。

// 风险分类定义已移入 internal/risk：候选策略生成与安全发现使用同一份
// 判定依据，留在本包会与 policygen 成环。此处保留别名，使既有消费方
// （handler、测试、前端契约）无需改动。
type (
	// RiskCategory 是高风险端口的风险来源。
	RiskCategory = risk.Category
	// RiskPosition 是风险连接所处的位置。
	RiskPosition = risk.Position
	// RiskPort 是风险端口清单中的一项。
	RiskPort = risk.Port
)

const (
	// RiskAdminPlaintext 是明文管理端口。
	RiskAdminPlaintext = risk.AdminPlaintext
	// RiskDatabase 是数据库直连端口。
	RiskDatabase = risk.Database
	// RiskFileShare 是文件共享端口。
	RiskFileShare = risk.FileShare
	// PositionEgressInternet 是出公网。
	PositionEgressInternet = risk.EgressInternet
	// PositionCrossNamespace 是跨 namespace。
	PositionCrossNamespace = risk.CrossNamespace
	// PositionSameNamespace 是同 namespace 内。
	PositionSameNamespace = risk.SameNamespace
)

// RiskPortCatalog 返回判定所用的完整端口清单，按端口号升序。
func RiskPortCatalog() []RiskPort { return risk.Catalog() }

// RiskyFlow 是一条落在风险端口上的流量，附带风险分类与位置。
type RiskyFlow struct {
	FlowRecord
	Category RiskCategory `json:"category"`
	Position RiskPosition `json:"position"`
	// PortName 是端口对应的协议名，供界面直接展示。
	PortName string `json:"portName"`
}

// EgressTarget 是一个公网出向目标的聚合。
type EgressTarget struct {
	Address   string  `json:"address"`
	Ports     []int32 `json:"ports"`
	FlowCount int     `json:"flowCount"`
	// AllowedCount 是其中判定为 ALLOW 的条数。
	//
	// 与总数分列：出公网被策略放行与被拦下是两件不同的事，只报总数
	// 会让一条畅通的外联与一条已被挡住的外联在报告里长得一样。
	AllowedCount int `json:"allowedCount"`
	// UnknownCount 是判定不出结果的条数。
	UnknownCount int `json:"unknownCount"`
}

// NakedPod 是一个未被任何 NetworkPolicy 选中的 Pod。
type NakedPod struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// MaxSecurityFindings 是这份报告里任意一个清单能装下的条数上限。
//
// 放在契约上而不是某个 Reader 内部：这个数随报告回显给调用方（每个清单的
// Limit），两个 Reader 必须回显同一个数。各写一份的话，界面上那句"共 N 条，
// 此处显示 M 条"会在换一个数据源之后开始说假话，而它没有任何症状。
const MaxSecurityFindings = 1000

// ListTruncation 是一个清单的条数回显：截断前的条数与生效上限。
//
// 与 FlowPage 的 Total / Limit 同形，且刻意沿用同两个名字：三个清单加上
// 流量列表是同一件事的四个实例，两种形状迟早会各自漂一点，而消费方要靠
// 它们说出同一句话。
//
// 两个字段恒定填写（Limit 永远大于 0），因此"空清单"与"没有这个字段"
// 可区分：前者是"我们查了，一条都没有"，是一个关于集群的结论；后者是一份
// 老响应或一个没填这个字段的 Reader，什么结论都支撑不了。
type ListTruncation struct {
	// Total 是截断前的条数。
	Total int `json:"total"`
	// Limit 是生效的条数上限。
	Limit int `json:"limit"`
}

// TruncateFindings 把一个清单截到 MaxSecurityFindings，并给出它的回显。
//
// 截断与回显必须由同一个函数产出：分成两处就会出现"截了、但 Total 报的是
// 截断后的长度"这种没有任何症状的错法 —— 那样的清单读起来就是"这个集群
// 只有这些"，而那是本平台唯一不能出的那种错。
//
// 调用方必须先排序再截：留下哪一批取决于内容，而不是取决于读取顺序，
// 否则同一次查询在两次刷新之间会换一批发现（与 sortFlowRecords 同一条理由）。
func TruncateFindings[T any](items []T) ([]T, ListTruncation) {
	echo := ListTruncation{Total: len(items), Limit: MaxSecurityFindings}
	if len(items) > MaxSecurityFindings {
		items = items[:MaxSecurityFindings]
	}
	return items, echo
}

// SecurityReport 是一个集群的安全发现汇总。
type SecurityReport struct {
	Cluster string `json:"cluster"`
	// Window 是流量类发现所用的时间窗。
	//
	// NakedPods 来自资产快照而非流量，不受本窗口约束 —— 两类数据
	// 时间语义不同，放在同一响应里必须说明，否则使用者会以为
	// "这段时间内有 6 个裸奔 Pod"。
	Window TimeWindow `json:"window"`
	// TrafficObserved 表示流量类发现是不是基于真实观测。
	//
	// **为 false 时，空的 RiskyFlows 不是「这个集群没有风险连接」，而是
	// 「我们一条流量都还没观测过」。** NakedPods 不受它影响 —— 那一栏来自
	// 资产快照，没有流量也答得出（design doc 2026-08-18 §4.2）。
	TrafficObserved bool        `json:"trafficObserved"`
	RiskyFlows      []RiskyFlow `json:"riskyFlows"`
	// RiskyFlowsTruncation 回显 RiskyFlows 截断前的条数与生效上限。
	//
	// 三个清单各回显各的，不合并成一个：被截的是哪一份决定了该怎么办 ——
	// 前两个清单缩小窗口就能拿全，NakedPods 来自锚点那一次快照，与流量
	// 窗口无关，缩窗对它毫无用处。
	RiskyFlowsTruncation    ListTruncation `json:"riskyFlowsTruncation"`
	EgressTargets           []EgressTarget `json:"egressTargets"`
	EgressTargetsTruncation ListTruncation `json:"egressTargetsTruncation"`
	NakedPods               []NakedPod     `json:"nakedPods"`
	NakedPodsTruncation     ListTruncation `json:"nakedPodsTruncation"`
	RiskPortCatalog         []RiskPort     `json:"riskPortCatalog"`
}

// Security 汇总一个集群的安全发现。集群不存在时返回错误。
func (r *FixtureReader) Security(ctx context.Context, clusterID string, window TimeWindow) (SecurityReport, error) {
	if !window.Valid() {
		return SecurityReport{}, ErrWindowRequired
	}
	c, ok := r.fleet.Cluster(clusterID)
	if !ok {
		return SecurityReport{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}
	// 与 Topology、Quality、PolicyPreview 一致：注册表是集群是否被平台
	// 管理的唯一判据。这是四者里暴露最敏感信息的一个——风险流量、出网
	// 目标、裸奔 Pod——漏掉这道门槛，未注册或已下线的集群会在别处都不
	// 可见、唯独在这里读得一清二楚。
	_, ok, err := r.registeredCluster(ctx, clusterID)
	if err != nil {
		return SecurityReport{}, err
	}
	if !ok {
		return SecurityReport{}, fmt.Errorf("%w: %s", ErrClusterNotFound, clusterID)
	}

	rep := SecurityReport{
		// 同 PolicyPreview：fixture 自带合成流量。
		TrafficObserved: true,
		Cluster:         clusterID,
		Window:          window,
		RiskyFlows:      []RiskyFlow{},
		EgressTargets:   []EgressTarget{},
		NakedPods:       []NakedPod{},
		RiskPortCatalog: RiskPortCatalog(),
	}

	flows, err := r.visibleFlows(ctx)
	if err != nil {
		return SecurityReport{}, err
	}

	targets := map[string]*EgressTarget{}
	for _, f := range flows {
		if !window.Contains(f.Flow.Timestamp) || !involvesCluster(f.Flow, clusterID) {
			continue
		}
		rec, _ := r.toFlowRecord(f)

		if rp, risky := risk.Lookup(f.Flow.Port); risky {
			rep.RiskyFlows = append(rep.RiskyFlows, RiskyFlow{
				FlowRecord: rec,
				Category:   rp.Category,
				Position:   riskPosition(f.Flow),
				PortName:   rp.Name,
			})
		}

		if isExternal(f.Flow.Dest) {
			t, seen := targets[f.Flow.Dest.IP]
			if !seen {
				t = &EgressTarget{Address: f.Flow.Dest.IP}
				targets[f.Flow.Dest.IP] = t
			}
			t.FlowCount++
			switch rec.Verdict {
			case string(replay.VerdictAllow):
				t.AllowedCount++
			case string(replay.VerdictUnknown):
				t.UnknownCount++
			}
			if !containsPort(t.Ports, f.Flow.Port) {
				t.Ports = append(t.Ports, f.Flow.Port)
			}
		}
	}

	for _, t := range targets {
		sort.Slice(t.Ports, func(i, j int) bool { return t.Ports[i] < t.Ports[j] })
		rep.EgressTargets = append(rep.EgressTargets, *t)
	}
	sort.Slice(rep.EgressTargets, func(i, j int) bool {
		return rep.EgressTargets[i].Address < rep.EgressTargets[j].Address
	})
	sort.Slice(rep.RiskyFlows, func(i, j int) bool { return rep.RiskyFlows[i].ID < rep.RiskyFlows[j].ID })

	// 裸奔 Pod 来自资产快照，与时间窗无关。hostNetwork Pod 不计入：
	// 它们不是"没被策略选中"，而是 NetworkPolicy 根本管不到（§6.2），
	// 混在一起会让两类完全不同的敞口显示成同一件事。
	for _, p := range c.Pods {
		if p.HostNetwork || podCoveredByPolicy(p, c.Policies) {
			continue
		}
		rep.NakedPods = append(rep.NakedPods, NakedPod{
			Cluster: p.ClusterID, Namespace: p.Namespace, Name: p.Name,
		})
	}
	sort.Slice(rep.NakedPods, func(i, j int) bool {
		a, b := rep.NakedPods[i], rep.NakedPods[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	// 合成数据集不越界，因此这三次调用只是把回显填上、一条都不截。仍然走
	// 同一个 TruncateFindings：两个 Reader 填的必须是同一句话，各写各的
	// 就会出现"演示集群从不回显、真集群才回显"这种前端分不清的差别。
	rep.RiskyFlows, rep.RiskyFlowsTruncation = TruncateFindings(rep.RiskyFlows)
	rep.EgressTargets, rep.EgressTargetsTruncation = TruncateFindings(rep.EgressTargets)
	rep.NakedPods, rep.NakedPodsTruncation = TruncateFindings(rep.NakedPods)

	return rep, nil
}

// riskPosition 判断一条风险流量所处的位置。
func riskPosition(f replay.Flow) RiskPosition {
	if isExternal(f.Dest) {
		return PositionEgressInternet
	}
	// 两端都要有 Pod 身份才谈得上"同 namespace"。身份缺失时保守归入
	// 跨 namespace：把一条来源不明的连接说成内部正常调用，是这份报告
	// 最不该犯的错。
	if f.Source.Pod != nil && f.Dest.Pod != nil &&
		f.Source.Pod.ClusterID == f.Dest.Pod.ClusterID &&
		f.Source.Pod.Namespace == f.Dest.Pod.Namespace {
		return PositionSameNamespace
	}
	return PositionCrossNamespace
}

// isExternal 报告端点是否为集群外地址：既无 Pod 身份，也不属于任何集群。
func isExternal(ep replay.Endpoint) bool {
	return ep.Pod == nil && ep.ClusterID == ""
}

func containsPort(ports []int32, p int32) bool {
	for _, x := range ports {
		if x == p {
			return true
		}
	}
	return false
}
