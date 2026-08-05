package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/imkerbos/Distill/internal/replay"
)

// 安全发现的类型与判定依据。
//
// 与判定引擎分开：这里回答的是"这条连接值不值得有人去看"，
// 而不是"这条连接通不通"。后者是 replay 的职责，前者是风险分类，
// 两者混在一起会让"策略挡住了"被读成"没有风险"——一次被 DENY 的
// 数据库直连仍然意味着有人在尝试连数据库。

// RiskCategory 是高风险端口的风险来源。封闭枚举。
type RiskCategory string

const (
	// RiskAdminPlaintext 是明文管理端口：拿到即等于拿到主机。
	RiskAdminPlaintext RiskCategory = "ADMIN_PLAINTEXT"
	// RiskDatabase 是数据库直连端口：绕过应用层的一切授权与审计。
	RiskDatabase RiskCategory = "DATABASE"
	// RiskFileShare 是文件共享端口：历史漏洞密集且常被横向移动利用。
	RiskFileShare RiskCategory = "FILE_SHARE"
)

// RiskPosition 是风险连接所处的位置。
//
// 必须与 RiskCategory 分开表达，不得合成一个分数：同一个 22 端口，
// 出公网、跨 namespace、同 namespace 内的处置方式完全不同，
// 合成之后使用者无法判断该找谁。
type RiskPosition string

const (
	// PositionEgressInternet 是出公网，风险最高的一类。
	PositionEgressInternet RiskPosition = "EGRESS_INTERNET"
	// PositionCrossNamespace 是跨 namespace。
	PositionCrossNamespace RiskPosition = "CROSS_NAMESPACE"
	// PositionSameNamespace 是同 namespace 内，通常属于正常架构。
	PositionSameNamespace RiskPosition = "SAME_NAMESPACE"
)

// RiskPort 是风险端口清单中的一项。
type RiskPort struct {
	Port     int32        `json:"port"`
	Name     string       `json:"name"`
	Category RiskCategory `json:"category"`
}

// riskPorts 是判定所用的封闭端口清单。
//
// 风险来自端口背后的协议语义，不来自"端口是否常见"。任何启发式规则
// （高位端口、非白名单端口）都会把正常架构标成风险，而一份充满误报的
// 安全报告最终会被整体忽略。
//
// 刻意不包含 9090 与 9100：它们是 Prometheus 与 node-exporter 的标准端口。
// 为了让报告有内容而给正常端口贴上风险标签，与伪造判定没有区别。
var riskPorts = map[int32]RiskPort{
	22:    {Port: 22, Name: "SSH", Category: RiskAdminPlaintext},
	23:    {Port: 23, Name: "Telnet", Category: RiskAdminPlaintext},
	3389:  {Port: 3389, Name: "RDP", Category: RiskAdminPlaintext},
	3306:  {Port: 3306, Name: "MySQL", Category: RiskDatabase},
	5432:  {Port: 5432, Name: "PostgreSQL", Category: RiskDatabase},
	6379:  {Port: 6379, Name: "Redis", Category: RiskDatabase},
	27017: {Port: 27017, Name: "MongoDB", Category: RiskDatabase},
	9200:  {Port: 9200, Name: "Elasticsearch", Category: RiskDatabase},
	445:   {Port: 445, Name: "SMB", Category: RiskFileShare},
}

// RiskPortCatalog 返回判定所用的完整端口清单，按端口号升序。
//
// 随报告一起返回：报告为空时，使用者必须能看到"我们查了哪些端口"。
// 缺了这份清单，一份空报告与一次根本没做的检查在界面上无法区分。
func RiskPortCatalog() []RiskPort {
	out := make([]RiskPort, 0, len(riskPorts))
	for _, p := range riskPorts {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

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

		if rp, risky := riskPorts[f.Flow.Port]; risky {
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
