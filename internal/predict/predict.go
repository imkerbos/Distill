// Package predict 回放候选策略，给出上线后的变化预测。
//
// 输出不是"放行多少条 / 阻断多少条"，而是四类变化。只报总数看不出
// 谁会被打断 —— 而"谁会被打断"是这个平台唯一必须答对的问题。
package predict

import (
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
)

// ChangeKind 是候选策略相对当前策略的变化类型。封闭枚举。
type ChangeKind string

const (
	// ChangeWouldBreak 表示现在放行、候选策略下阻断。
	//
	// 上线即生产阻断。这一类的绝对数是整份报告唯一真正要人看的数字。
	ChangeWouldBreak ChangeKind = "WOULD_BREAK"
	// ChangeWouldOpen 表示现在阻断、候选策略下放行。
	//
	// 敌方面扩大。候选策略不该无意放开东西。
	ChangeWouldOpen ChangeKind = "WOULD_OPEN"
	// ChangeUnchanged 表示判定一致。
	ChangeUnchanged ChangeKind = "UNCHANGED"
	// ChangeUnknown 表示候选策略下判不出。
	ChangeUnknown ChangeKind = "UNKNOWN"
)

// allChangeKinds 是枚举的唯一登记处。
var allChangeKinds = []ChangeKind{
	ChangeWouldBreak, ChangeWouldOpen, ChangeUnchanged, ChangeUnknown,
}

// AllChangeKinds 返回全部已登记的变化类型。
func AllChangeKinds() []ChangeKind {
	out := make([]ChangeKind, len(allChangeKinds))
	copy(out, allChangeKinds)
	return out
}

// Valid 判断该变化类型是否已登记。
func (c ChangeKind) Valid() bool {
	for _, known := range allChangeKinds {
		if c == known {
			return true
		}
	}
	return false
}

// ChangedFlow 是一条流量在候选策略下的变化。
type ChangedFlow struct {
	// FlowID 是流量标识。
	FlowID string `json:"flowId"`
	// SourceLabel 与 DestLabel 是端点的展示名。
	SourceLabel string `json:"sourceLabel"`
	DestLabel   string `json:"destLabel"`
	// Protocol 与 Port 是连接的传输层信息。
	Protocol string `json:"protocol"`
	Port     int32  `json:"port"`
	// Current 是当前策略下的判定。
	Current string `json:"current"`
	// Predicted 是候选策略下的判定。
	Predicted string `json:"predicted"`
	// UnknownReason 在 Predicted 为 UNKNOWN 时说明原因。
	UnknownReason string `json:"unknownReason"`
	// Confidence 是预测结论的可信度。
	Confidence string `json:"confidence"`
	// CrossCluster 表示这是一条跨集群连接。
	CrossCluster bool `json:"crossCluster"`
	// Unmanaged 表示主体不受 NetworkPolicy 管控。
	Unmanaged bool `json:"unmanaged"`
}

// Report 是一次预测的完整结果。
type Report struct {
	// Changes 是四类变化各自的完整连接清单。
	//
	// 保留完整清单而非只留计数（spec §8.1）：一个"会打断 12 条"的数字
	// 无法让任何人去修，必须能点开看到是哪 12 条。
	Changes map[ChangeKind][]ChangedFlow `json:"changes"`
	// Counts 是四类变化的条数。
	Counts map[ChangeKind]int `json:"counts"`
	// UnknownComposition 是 UNKNOWN 按原因的构成，绝对数。
	UnknownComposition map[string]int `json:"unknownComposition"`
	// TrustedCount 与 DegradedCount 是预测结论的可信度分布。
	//
	// 必须与结论分列：90% DEGRADED 的预测不应与 90% TRUSTED 同等对待。
	TrustedCount  int `json:"trustedCount"`
	DegradedCount int `json:"degradedCount"`
	// CrossClusterCount 是跨集群连接数，已知敞口规模。
	CrossClusterCount int `json:"crossClusterCount"`
	// UnmanagedCount 是不受 NetworkPolicy 管控的连接数。
	UnmanagedCount int `json:"unmanagedCount"`
	// TotalEvaluated 是参与预测的连接总数，四类计数之和。
	TotalEvaluated int `json:"totalEvaluated"`
}

// Input 是一次预测所需的全部输入。
type Input struct {
	// ClusterID 是预测的目标集群。
	ClusterID string
	// Policies 是候选策略集，只含启用规则。
	Policies []networkingv1.NetworkPolicy
	// Namespaces 是该集群的命名空间快照。
	Namespaces []replay.NamespaceRef
	// CCNPPresent 表示该集群存在 Cilium 策略，预测结论需降级。
	CCNPPresent bool
	// Observations 是带当前判定的观测流量。
	Observations []policygen.Observation
	// Label 把端点渲染成展示名；为空时用 IP。
	//
	// 由调用方注入而非在本包实现：展示名的格式属于消费方的呈现决策，
	// 写死在这里会让预测报告与流量列表用两套不同的名字指同一个 Pod。
	Label func(replay.Endpoint) string
}

// Run 回放候选策略并给出变化预测。纯函数。
func Run(in Input) Report {
	ev := replay.NewEvaluator(in.ClusterID, in.Policies, in.Namespaces,
		replay.WithCCNPPresent(in.CCNPPresent))

	rep := Report{
		Changes:            map[ChangeKind][]ChangedFlow{},
		Counts:             map[ChangeKind]int{},
		UnknownComposition: map[string]int{},
	}
	for _, k := range allChangeKinds {
		rep.Changes[k] = []ChangedFlow{}
		rep.Counts[k] = 0
	}

	for _, o := range in.Observations {
		predicted := ev.Evaluate(o.Flow)
		kind := classifyChange(o.Decision.Verdict, predicted.Verdict)

		rep.Counts[kind]++
		rep.TotalEvaluated++
		switch predicted.Confidence {
		case replay.ConfidenceDegraded:
			rep.DegradedCount++
		default:
			rep.TrustedCount++
		}
		if predicted.CrossCluster {
			rep.CrossClusterCount++
		}
		if predicted.Reason.Unmanaged {
			rep.UnmanagedCount++
		}
		if kind == ChangeUnknown {
			rep.UnknownComposition[string(predicted.UnknownReason)]++
		}

		rep.Changes[kind] = append(rep.Changes[kind], ChangedFlow{
			FlowID:        o.FlowID,
			SourceLabel:   label(in.Label, o.Flow.Source),
			DestLabel:     label(in.Label, o.Flow.Dest),
			Protocol:      string(o.Flow.Protocol),
			Port:          o.Flow.Port,
			Current:       string(o.Decision.Verdict),
			Predicted:     string(predicted.Verdict),
			UnknownReason: string(predicted.UnknownReason),
			Confidence:    string(predicted.Confidence),
			CrossCluster:  predicted.CrossCluster,
			Unmanaged:     predicted.Reason.Unmanaged,
		})
	}
	return rep
}

// classifyChange 判定一条流量的变化类型。
//
// UNKNOWN 优先于其余三类：候选策略下判不出结论时，说它"没变"是在
// 用一个不存在的确定性掩盖数据缺口。
func classifyChange(current, predicted replay.Verdict) ChangeKind {
	if predicted == replay.VerdictUnknown {
		return ChangeUnknown
	}
	switch {
	case current == replay.VerdictAllow && predicted == replay.VerdictDeny:
		return ChangeWouldBreak
	case current == replay.VerdictDeny && predicted == replay.VerdictAllow:
		return ChangeWouldOpen
	default:
		return ChangeUnchanged
	}
}

// label 渲染端点展示名，未注入渲染函数时回落到 IP。
func label(fn func(replay.Endpoint) string, ep replay.Endpoint) string {
	if fn != nil {
		return fn(ep)
	}
	return ep.IP
}
