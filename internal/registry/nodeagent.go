package registry

import "github.com/imkerbos/Distill/internal/snapshot"

// NodeAgentRegistration 是一个需要被放行的节点级 agent。
//
// **登记出来的，不是推导出来的**（design doc 2026-08-18-node-agent-applicability §3）：
// agent 连不连工作负载、连哪个端口，写在它自己的配置里，不在任何资产里。
// 平台看得见集群里有哪些 hostNetwork DaemonSet，但看不见它们往哪连。
//
// 推断错的方向是危险的那一侧：把一个真的需要放行的 agent 判成"不需要"，
// 下发之后监控静默中断，而在事故发生时才显现。
type NodeAgentRegistration struct {
	// Namespace 是 agent 所在命名空间。
	Namespace string `json:"namespace"`
	// App 是 agent 的 app 标签值，非 hostNetwork 时用作 podSelector。
	App string `json:"app"`
	// HostNetwork 表示这个 agent 使用宿主网络。
	//
	// 为 true 时推导必须走 node CIDR 而非 podSelector：源地址是节点 IP，
	// podSelector 永远选不中它（V4 spec §6.2）。写成 podSelector 会得到
	// 一条看起来正确、实际从不匹配的规则。
	HostNetwork bool `json:"hostNetwork"`
	// TargetPort 是 agent 访问工作负载的目标端口。
	//
	// 必填。**不设默认值** —— 一条放行到猜出来的端口的规则，看起来齐备、
	// 实际什么都没放行（derive_infra.go 的注释）。
	TargetPort int32 `json:"targetPort"`
}

// ValidateNodeAgent 校验一条节点 agent 登记。
func ValidateNodeAgent(a NodeAgentRegistration) error {
	if a.Namespace == "" {
		return invalid("节点 agent 必须给出命名空间")
	}
	if a.App == "" {
		return invalid("节点 agent 必须给出 app 标签值：非 hostNetwork 的 agent 靠它构造 podSelector")
	}
	if a.TargetPort <= 0 || a.TargetPort > 65535 {
		return invalidf("节点 agent 的目标端口 %d 超出 1-65535 范围：这个端口只有人知道，平台不猜", a.TargetPort)
	}
	return nil
}

// NodeAgentSnapshots 把登记转成推导所需的快照视图。
//
// 与 APIServerSnapshots / ScrapeTargetSnapshots 同形：推导层只认
// snapshot.NodeAgent，不知道数据来自登记还是别的地方。
func (c Cluster) NodeAgentSnapshots() []snapshot.NodeAgent {
	if len(c.NodeAgents) == 0 {
		return nil
	}
	out := make([]snapshot.NodeAgent, 0, len(c.NodeAgents))
	for _, a := range c.NodeAgents {
		out = append(out, snapshot.NodeAgent{
			ClusterID:   c.ID,
			Namespace:   a.Namespace,
			App:         a.App,
			HostNetwork: a.HostNetwork,
			TargetPort:  a.TargetPort,
		})
	}
	return out
}
