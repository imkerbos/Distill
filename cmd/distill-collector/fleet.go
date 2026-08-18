package main

import (
	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/fleet"
	"github.com/imkerbos/Distill/internal/registry"
)

// fleetRegistry 转发到 internal/fleet。
//
// 这段逻辑搬走了，因为 PUSH 模式下同一次判定发生在平台收下推送的时候
// （design doc 2026-08-18 §3.4）—— 两个消费方，就不该有两份实现。
// 这里留一个转发而不是让调用点直接调：本包的测试与调用点都按这个名字写。
func fleetRegistry(clusters []registry.Cluster) (reg *cluster.Registry, unusable []string) {
	return fleet.FromRegistry(clusters)
}
