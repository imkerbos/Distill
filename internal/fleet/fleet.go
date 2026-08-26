// Package fleet 把注册表里的集群登记转成网段判定所需的形态。
//
// 单独成包，是因为它有两个消费方：PULL 模式的采集器在采完之后判定归属，
// 而 PUSH 模式下这件事发生在平台收下推送的时候（design doc 2026-08-18 §3.4）。
// 抄两份的那一天，只会是其中一处修了网段解析而另一处没有 —— 而两处的
// 分歧不会报错，只会让同一个地址在两条路径上得到不同的归属。
package fleet

import (
	"net/netip"
	"strings"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/registry"
)

// FromRegistry 把注册表里的集群登记转成网段判定所需的形态，
// 并返回登记不可用、因而判定会退化的集群 ID。
//
// 单值包成单元素 slice、ServiceCIDRs 留空：registry.Cluster 的网段字段
// 目前是单个 string，也没有 Service 网段字段，多值化是一次独立的迁移
// （spec §2.3）。于是 Service ClusterIP 会被判成 UNKNOWN 而不是 EXTERNAL
// —— 那是 internal/cluster 那条"没登记就不许答 EXTERNAL"的直接后果，
// 不是这里漏了什么。把"我们没登记"说成"它在集群外"才是错的。
//
// 网段解析不了的集群照样进登记表，只是没有网段：让它整个消失会让
// Classify 把它的 Pod IP 判成 EXTERNAL 或另一个集群的，而带着空网段留下
// 来只会让判定退化成 UNKNOWN 并附上缺口原因。**失败方向朝"答不出"，
// 不朝"自信地答错"。**
// FromRegistry 见包注释。
func FromRegistry(clusters []registry.Cluster) (reg *cluster.Registry, unusable []string) {
	out := make([]cluster.Cluster, 0, len(clusters))
	for _, c := range clusters {
		pods, podOK := parsePrefixes(c.PodCIDR)
		nodes, nodeOK := parsePrefixes(c.NodeCIDR)
		if !podOK || !nodeOK {
			unusable = append(unusable, c.ID)
		}
		out = append(out, cluster.Cluster{
			ID:        c.ID,
			PodCIDRs:  pods,
			NodeCIDRs: nodes,
		})
	}
	return cluster.NewRegistry(out), unusable
}

// parsePrefixes 解析登记里的网段，支持逗号分隔的多段。
//
// **双栈集群的每个 Pod 有两个地址**，一个 IPv4、一个 IPv6。只登记得下一个的
// 话，走另一个协议族的连接会落进 EXTERNAL —— 平台把它当成出公网，于是生成
// 一条 ipBlock 规则而不是 selector 规则，放行面比实际需要的宽得多。
//
// 用逗号分隔而不是加一个新字段：单段登记原样继续工作，绝大多数集群是单栈，
// 不该因为支持多段就要求他们改写已有登记。
//
// **一段解析不出就整条作废**，不是"能用几段算几段"：部分可用的登记会让一部分
// 地址落进 UNKNOWN，而运维看到的是"大部分都对"，那条写错的网段要等到某条
// 流量归属错了才暴露。整条作废会立刻出现在「网段登记坏掉」的告警里。
//
// ok 为 false 表示这个登记用不了（空、有空段、或任一段不是合法网段）。
func parsePrefixes(cidrs string) (prefixes []netip.Prefix, ok bool) {
	if strings.TrimSpace(cidrs) == "" {
		return nil, false
	}
	parts := strings.Split(cidrs, ",")
	out := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		// 空段多半是手滑留下的多余逗号。静默忽略会让人以为填对了，
		// 而他少填的那一段正是双栈里的另一半。
		seg := strings.TrimSpace(part)
		if seg == "" {
			return nil, false
		}
		p, err := netip.ParsePrefix(seg)
		if err != nil {
			return nil, false
		}
		out = append(out, p)
	}
	return out, true
}
