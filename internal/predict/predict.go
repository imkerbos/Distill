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
	// ChangeUnchanged 表示两侧判定一致，且都不是 UNKNOWN。
	ChangeUnchanged ChangeKind = "UNCHANGED"
	// ChangeUnknown 表示当前判定或候选策略判定中任一侧判不出。
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
	// UnknownReason 在任一侧判定为 UNKNOWN 时说明原因，取该侧的原因。
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
	// UnratedCount 是可信度取值不在枚举内的连接数，正常恒为 0。
	//
	// 单列而非并进 TrustedCount：三者之和必须等于 TotalEvaluated，
	// 若把枚举外取值折进 TRUSTED，回放层新增一档可信度时这里会静默地
	// 把它报成"可信"，而分布看上去仍然自洽。
	UnratedCount int `json:"unratedCount"`
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
	// EvalOptions 是「这个集群该怎么求值」的完整描述，由调用方一次算出。
	//
	// 收成一份选项而不是几个布尔，是因为 dry-run 与 /flows 那一屏必须用
	// **同一个模型**解释同一个集群。此前这里是一个 ForeignPlane 布尔，而
	// 判定那一侧还额外接了精确降级范围与 AdminNetworkPolicy —— 两边各自
	// 拼装，dry-run 就此看不见 ANP：一条被 ANP Deny 拦着的连接，/flows 说
	// DENY，dry-run 的基线说 ALLOW，于是 WOULD_BREAK 算的是一次不存在的
	// 中断，而那个数是写回门禁的判据。
	//
	// 代码里那句「对账器与读栈同源」要的正是这件事：两条路必须走同一个引擎。
	// 传选项让它在类型上成立 —— 少传一项的调用方会看见自己少传了什么，
	// 而不是少传了一个没人记得的布尔。
	EvalOptions []replay.Option
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
	ev := replay.NewEvaluator(in.ClusterID, in.Policies, in.Namespaces, in.EvalOptions...)

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
		reason := unknownReasonOf(o.Decision, predicted)

		rep.Counts[kind]++
		rep.TotalEvaluated++
		switch predicted.Confidence {
		case replay.ConfidenceTrusted:
			rep.TrustedCount++
		case replay.ConfidenceDegraded:
			rep.DegradedCount++
		default:
			// 枚举外的取值单独计数，不并进 TRUSTED：把一个说不清可信度的
			// 结论算成可信，是往让人放心的方向报数，正是本平台不许犯的错。
			rep.UnratedCount++
		}
		if predicted.CrossCluster {
			rep.CrossClusterCount++
		}
		if predicted.Reason.Unmanaged {
			rep.UnmanagedCount++
		}
		if kind == ChangeUnknown {
			rep.UnknownComposition[string(reason)]++
		}

		rep.Changes[kind] = append(rep.Changes[kind], ChangedFlow{
			FlowID:        o.FlowID,
			SourceLabel:   label(in.Label, o.Flow.Source),
			DestLabel:     label(in.Label, o.Flow.Dest),
			Protocol:      string(o.Flow.Protocol),
			Port:          o.Flow.Port,
			Current:       string(o.Decision.Verdict),
			Predicted:     string(predicted.Verdict),
			UnknownReason: string(reason),
			Confidence:    string(predicted.Confidence),
			CrossCluster:  predicted.CrossCluster,
			Unmanaged:     predicted.Reason.Unmanaged,
		})
	}
	return rep
}

// classifyChange 判定一条流量的变化类型。
//
// 两侧的 UNKNOWN 都优先于其余三类（spec §5）：任一侧判不出结论时，
// 说它"没变"是在用一个不存在的确定性掩盖数据缺口。
//
// current 一侧尤其不能漏：current=UNKNOWN + predicted=DENY 落进
// UNCHANGED，界面上会以「判定结论保持一致」呈现 —— 那等于宣称我们
// 知道这条连接原本是通的，而事实是我们从来就没判出来过。
func classifyChange(current, predicted replay.Verdict) ChangeKind {
	if predicted == replay.VerdictUnknown {
		return ChangeUnknown
	}
	if current == replay.VerdictUnknown {
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

// unknownReasonOf 取把这条流量推进 UNKNOWN 的那一侧的原因。
//
// 两侧的缺口成因不同，要修的子系统也不同：predicted 侧的 UNKNOWN 来自
// 候选策略回放，current 侧的来自当前判定。记错一侧会让缺口挂到一个与它
// 无关的成因上，构成表既指不回源头，也不再等于 UNKNOWN 总数。
//
// **两侧都判不出时以 current（归属层）为准。** 这一支不是对称的：求值引擎
// 只看得见 `endpoint.Pod == nil`，它不知道为什么解不开身份，只能给一个占位
// （evaluator 那一处固定返回 SNAPSHOT_MISSING）；而归属层四路分类过 ——
// 本集群节点、LB 入口地址、集群外地址、IP 复用分不开。取占位值等于把四类
// 压成一类，且压成唯一一个会把人引去查采集的那个（UAT 实测：2259 条 UNKNOWN
// 里 1412 条这样标错，而其中 375 条 LB_INGRESS_ADDRESS 的枚举注释写着
// 「根本没有什么该采而没采的东西」）。
//
// current 判得出结论、只有 predicted 是 UNKNOWN 时仍取 predicted：那类缺口
// 是候选集自己引入的，指向的子系统是策略生成，不是归属。
func unknownReasonOf(current, predicted replay.Decision) replay.UnknownReason {
	if current.Verdict == replay.VerdictUnknown && current.UnknownReason != replay.ReasonNone {
		return current.UnknownReason
	}
	if predicted.Verdict == replay.VerdictUnknown {
		return predicted.UnknownReason
	}
	if current.Verdict == replay.VerdictUnknown {
		return current.UnknownReason
	}
	return ""
}

// label 渲染端点展示名，未注入渲染函数时回落到 IP。
func label(fn func(replay.Endpoint) string, ep replay.Endpoint) string {
	if fn != nil {
		return fn(ep)
	}
	return ep.IP
}
