// Package fleet 把注册表里的集群登记转成网段判定所需的形态。
//
// 单独成包，是因为它有两个消费方：PULL 模式的采集器在采完之后判定归属，
// 而 PUSH 模式下这件事发生在平台收下推送的时候（design doc 2026-08-18 §3.4）。
// 抄两份的那一天，只会是其中一处修了网段解析而另一处没有 —— 而两处的
// 分歧不会报错，只会让同一个地址在两条路径上得到不同的归属。
package fleet

import (
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
		pods, podOK := cluster.ParsePrefixes(c.PodCIDR)
		nodes, nodeOK := cluster.ParsePrefixes(c.NodeCIDR)
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
