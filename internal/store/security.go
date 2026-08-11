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
	RiskCategory = risk.RiskCategory
	// RiskPosition 是风险连接所处的位置。
	RiskPosition = risk.RiskPosition
	// RiskPort 是风险端口清单中的一项。
	RiskPort = risk.RiskPort
)

const (
	// RiskAdminPlaintext 是明文管理端口。
	RiskAdminPlaintext = risk.RiskAdminPlaintext
	// RiskDatabase 是数据库直连端口。
	RiskDatabase = risk.RiskDatabase
	// RiskFileShare 是文件共享端口。
	RiskFileShare = risk.RiskFileShare
	// PositionEgressInternet 是出公网。
	PositionEgressInternet = risk.PositionEgressInternet
	// PositionCrossNamespace 是跨 namespace。
	PositionCrossNamespace = risk.PositionCrossNamespace
	// PositionSameNamespace 是同 namespace 内。
	PositionSameNamespace = risk.PositionSameNamespace
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

// SecurityReport 是一个集群的安全发现汇总。
type SecurityReport struct {
	Cluster string `json:"cluster"`
	// Window 是流量类发现所用的时间窗。
	//
	// NakedPods 来自资产快照而非流量，不受本窗口约束 —— 两类数据
	// 时间语义不同，放在同一响应里必须说明，否则使用者会以为
	// "这段时间内有 6 个裸奔 Pod"。
	Window          TimeWindow     `json:"window"`
	RiskyFlows      []RiskyFlow    `json:"riskyFlows"`
	EgressTargets   []EgressTarget `json:"egressTargets"`
	NakedPods       []NakedPod     `json:"nakedPods"`
	RiskPortCatalog []RiskPort     `json:"riskPortCatalog"`
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

	rep := SecurityReport{
		Cluster:         clusterID,
		Window:          window,
		RiskyFlows:      []RiskyFlow{},
		EgressTargets:   []EgressTarget{},
		NakedPods:       []NakedPod{},
		RiskPortCatalog: RiskPortCatalog(),
	}

	targets := map[string]*EgressTarget{}
	for _, f := range r.fleet.Flows {
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
