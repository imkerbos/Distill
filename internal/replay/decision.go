// Package replay 实现标准 NetworkPolicy 的精确求值。
//
// 本包是纯函数：不做任何 I/O，不依赖集群或 GCP 环境。它是全平台
// 正确性的根 —— 一次错误的判定会直接变成一次错误的策略推荐。
package replay

// Verdict 是回放引擎对一条 flow 的判定结论。
type Verdict string

const (
	// VerdictAllow 表示该连接被策略放行。
	VerdictAllow Verdict = "ALLOW"
	// VerdictDeny 表示该连接被策略阻断。
	VerdictDeny Verdict = "DENY"
	// VerdictUnknown 表示数据不足以判定。
	VerdictUnknown Verdict = "UNKNOWN"
)

// Confidence 是判定结论的可信度，与 Verdict 相互独立。
//
// 两者必须分离：mesh 内流量与存在 CCNP 的集群属于"能判定但不可信"，
// 合并成单一字段会让这类结论被当作可信结果用于策略推荐。
type Confidence string

const (
	// ConfidenceTrusted 表示身份完整、无系统性干扰，可用于策略推荐。
	ConfidenceTrusted Confidence = "TRUSTED"
	// ConfidenceDegraded 表示存在 mesh 或 CCNP 干扰，仅供展示，
	// 不得作为策略推荐的依据。
	ConfidenceDegraded Confidence = "DEGRADED"
)

// UnknownReason 是无法判定的原因，取值为封闭枚举。
//
// 禁止自由文本：平台指标需要统计 UNKNOWN 的构成，据此定位应修复的
// 环节（是快照频率不够，还是日志限流该调）。自由文本无法聚合。
type UnknownReason string

const (
	// ReasonNone 表示不存在未知原因。
	ReasonNone UnknownReason = ""
	// ReasonSnapshotMissing 表示缺少对应时刻的资产快照。
	ReasonSnapshotMissing UnknownReason = "SNAPSHOT_MISSING"
	// ReasonIPAmbiguous 表示同集群内 IP 复用且时间上不可区分。
	ReasonIPAmbiguous UnknownReason = "IP_AMBIGUOUS"
	// ReasonClusterAmbiguous 表示跨集群网段重叠，归属不唯一。
	ReasonClusterAmbiguous UnknownReason = "CLUSTER_AMBIGUOUS"
	// ReasonIdentityLostMesh 与 ReasonCCNPPresent 是 DEGRADED 通道的标记位，
	// 保留给可信度路径使用，本引擎永远不会把它们写进 Decision.UnknownReason。
	//
	// 依据 spec §6.4：mesh 与 CCNP 不是"无法判定"，而是可检测的系统性降级——
	// 仍然给出 ALLOW/DENY 结论、仍然展示拓扑，只是 confidence=DEGRADED、
	// 不得作为策略推荐的依据。把它们当成 UNKNOWN 原因输出等于把整个 mesh
	// namespace 判死，正是 V3 的错误。二者登记在枚举里是为了让下游（看板、
	// 指标聚合）有一个封闭的取值全集，不代表本层会产出它们。

	// ReasonIdentityLostMesh 表示 sidecar 导致源身份丢失。
	ReasonIdentityLostMesh UnknownReason = "IDENTITY_LOST_MESH"
	// ReasonCCNPPresent 表示存在 CCNP，标准 NetworkPolicy 结论不可靠。
	ReasonCCNPPresent UnknownReason = "CCNP_PRESENT"
	// ReasonNATTranslated 表示地址被转换，无法还原原始主体。
	ReasonNATTranslated UnknownReason = "NAT_TRANSLATED"
	// ReasonExternalNoIdentity 表示公网流量无可归属主体。
	ReasonExternalNoIdentity UnknownReason = "EXTERNAL_NO_IDENTITY"
	// ReasonNamedPortUnresolved 表示命名端口无法解析为具体端口号。
	ReasonNamedPortUnresolved UnknownReason = "NAMED_PORT_UNRESOLVED"
	// ReasonLogSampledOut 表示日志采样或限流导致记录缺失。
	ReasonLogSampledOut UnknownReason = "LOG_SAMPLED_OUT"
	// ReasonPolicyMalformed 表示策略对象本身无法解析，例如 ipBlock 的
	// CIDR 或 except 条目格式非法。与其它原因不同，这不是数据缺失，
	// 而是策略写错了 —— 修复方式是改 YAML，不是补数据。
	ReasonPolicyMalformed UnknownReason = "POLICY_MALFORMED"
	// ReasonAdminPolicyUnsupported 表示这条管理面策略用到了平台还解释不了
	// 的东西 —— 当前只有 egress 的 nodes peer，它要节点标签，而平台没采。
	//
	// 与 POLICY_MALFORMED 分开：那一条说的是策略写错了，处置是去改策略；
	// 这一条说的是平台还不会读，策略本身完全合法，处置是去补平台的能力。
	// 合成一条会把一批人送去审一份没有问题的 YAML。
	ReasonAdminPolicyUnsupported UnknownReason = "ADMIN_POLICY_UNSUPPORTED"
	// ReasonAdminPriorityAmbiguous 表示两条 AdminNetworkPolicy 用了同一个
	// priority，且都选中了这个主体。
	//
	// 按 API 的原文，这时集群的行为是**未定义**的：谁先生效取决于实现。
	// 平台不替它挑一个 —— 挑中的那一半时间会给出一个自信的错答案。
	// 处置是去把其中一条的 priority 改开。
	ReasonAdminPriorityAmbiguous UnknownReason = "ADMIN_PRIORITY_AMBIGUOUS"
)

// allUnknownReasons 是枚举的唯一登记处。新增原因必须同步登记，
// 否则 Valid 会拒绝它，测试会失败。
var allUnknownReasons = []UnknownReason{
	ReasonSnapshotMissing,
	ReasonIPAmbiguous,
	ReasonClusterAmbiguous,
	ReasonIdentityLostMesh,
	ReasonCCNPPresent,
	ReasonNATTranslated,
	ReasonExternalNoIdentity,
	ReasonNamedPortUnresolved,
	ReasonLogSampledOut,
	ReasonPolicyMalformed,
	ReasonAdminPolicyUnsupported,
	ReasonAdminPriorityAmbiguous,
}

// AllUnknownReasons 返回全部已登记的未知原因。
func AllUnknownReasons() []UnknownReason {
	out := make([]UnknownReason, len(allUnknownReasons))
	copy(out, allUnknownReasons)
	return out
}

// Valid 判断该原因是否已登记。ReasonNone 视为合法。
func (r UnknownReason) Valid() bool {
	if r == ReasonNone {
		return true
	}
	for _, known := range allUnknownReasons {
		if r == known {
			return true
		}
	}
	return false
}

// escalate 在多个未决原因并存时选出优先级最高的一个。
//
// 顺序反映"该去修哪里"的紧急程度：策略写错 > 快照缺失 > 命名端口未解析。
// 用固定优先级而非后写覆盖，是因为 §17 要统计 UNKNOWN 构成，让遍历顺序
// 决定分类会让这个指标不可复现——同一份策略集换个切片顺序就换一个分类。
func escalate(cur, next UnknownReason) UnknownReason {
	if unknownReasonRank(next) > unknownReasonRank(cur) {
		return next
	}
	return cur
}

// unknownReasonRank 给本层可产出的原因排定优先级，未产出的一律为 0。
func unknownReasonRank(r UnknownReason) int {
	switch r {
	// 这两条排在 POLICY_MALFORMED 之上：它们说的是「这一段根本没算」，
	// 而 POLICY_MALFORMED 说的是「某条策略读不动、但别的还在算」。前者
	// 一旦成立，后面两段该不该执行都无从谈起，是更根本的不知道。
	case ReasonAdminPriorityAmbiguous:
		return 5
	case ReasonAdminPolicyUnsupported:
		return 4
	case ReasonPolicyMalformed:
		return 3
	case ReasonSnapshotMissing:
		return 2
	case ReasonNamedPortUnresolved:
		return 1
	default:
		return 0
	}
}

// Direction 是策略作用方向。
type Direction string

const (
	// DirectionIngress 是入向。
	DirectionIngress Direction = "INGRESS"
	// DirectionEgress 是出向。
	DirectionEgress Direction = "EGRESS"
)

// Reason 是结构化的判定理由，供判定解释器向用户说明结论由来。
type Reason struct {
	// Direction 是决定结论的方向。
	Direction Direction
	// Isolated 表示该方向上主体已进入隔离状态。
	Isolated bool
	// Unmanaged 表示主体不受 NetworkPolicy 管控，如 hostNetwork Pod。
	Unmanaged bool
	// MatchedPolicy 是命中的策略，格式 namespace/name；未命中为空。
	MatchedPolicy string
	// MatchedRuleIdx 是命中的规则下标；未命中为 -1。
	MatchedRuleIdx int
	// Detail 是补充说明，仅用于展示，不参与统计。
	Detail string
}

// NewReason 构造指定方向的空理由，未命中下标初始化为 -1。
func NewReason(dir Direction) Reason {
	return Reason{Direction: dir, MatchedRuleIdx: -1}
}

// Decision 是回放引擎对单条 flow 的完整判定输出。
type Decision struct {
	// Verdict 是判定结论。
	Verdict Verdict
	// Confidence 是结论可信度。
	Confidence Confidence
	// Reason 是结构化理由。
	Reason Reason
	// UnknownReason 在 Verdict 为 UNKNOWN 时说明原因。
	UnknownReason UnknownReason
	// CrossCluster 表示这是一条跨集群连接。
	CrossCluster bool
}
