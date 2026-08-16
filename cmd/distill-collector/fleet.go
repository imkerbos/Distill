package main

import (
	"net/netip"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/registry"
)

// fleetRegistry 把注册表里的集群登记转成网段判定所需的形态，
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
func fleetRegistry(clusters []registry.Cluster) (reg *cluster.Registry, unusable []string) {
	out := make([]cluster.Cluster, 0, len(clusters))
	for _, c := range clusters {
		pods, podOK := singlePrefix(c.PodCIDR)
		nodes, nodeOK := singlePrefix(c.NodeCIDR)
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

// singlePrefix 把一个 CIDR 字符串包成单元素切片；ok 为 false 表示这个
// 登记用不了（空或不是合法网段）。
func singlePrefix(cidr string) (prefixes []netip.Prefix, ok bool) {
	if cidr == "" {
		return nil, false
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, false
	}
	return []netip.Prefix{p}, true
}
