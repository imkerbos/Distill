package collect

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// collectPods 采集全部 Pod，并就地判定 IP 归属、mesh 状态与顶层控制器。
func (c *Collector) collectPods(ctx context.Context, obs *snapshot.Observation, rsOwners map[string]ownerRef) error {
	return eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
		list, err := c.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list pods in %s: %w", c.clusterID, err)
		}
		for i := range list.Items {
			pod, warnings := c.toPod(&list.Items[i], rsOwners)
			obs.Pods = append(obs.Pods, pod)
			obs.Warnings = append(obs.Warnings, warnings...)
		}
		return list.Continue, nil
	})
}

// toPod 把一个 Pod 对象转成快照记录，并产出它引发的告警。
func (c *Collector) toPod(p *corev1.Pod, rsOwners map[string]ownerRef) (snapshot.Pod, []snapshot.Warning) {
	var warnings []snapshot.Warning
	subject := p.Namespace + "/" + p.Name

	// mesh 判定取实际容器列表，不取 namespace 标签：注入是否真的发生
	// 只有 Pod 自己知道。initContainers 一并计入 —— 有的注入形态只留
	// init 容器痕迹，漏掉它会让一个身份失真的 Pod 不被标记降级。
	names := make([]string, 0, len(p.Spec.Containers)+len(p.Spec.InitContainers))
	for _, ct := range p.Spec.Containers {
		names = append(names, ct.Name)
	}
	for _, ct := range p.Spec.InitContainers {
		names = append(names, ct.Name)
	}
	mesh := cluster.DetectPodMesh(names)

	owner := controllerOf(p.OwnerReferences)
	workload, resolved := resolveWorkload(p.Namespace, owner, rsOwners)
	if !resolved {
		warnings = append(warnings, snapshot.Warning{
			Kind:    snapshot.WarningWorkloadUnresolved,
			Subject: subject,
			Detail:  "owner ReplicaSet " + workload.Name + " was not present in this run",
		})
	}

	snap := snapshot.Pod{
		ClusterID:      c.clusterID,
		Namespace:      p.Namespace,
		Name:           p.Name,
		UID:            string(p.UID),
		Phase:          string(p.Status.Phase),
		IP:             p.Status.PodIP,
		Labels:         p.Labels,
		HostNetwork:    p.Spec.HostNetwork,
		NodeName:       p.Spec.NodeName,
		ServiceAccount: p.Spec.ServiceAccountName,
		OwnerKind:      owner.Kind,
		OwnerName:      owner.Name,
		WorkloadKind:   workload.Kind,
		WorkloadName:   workload.Name,
		InMesh:         mesh.InMesh,
		MeshSource:     mesh.Source,
		MeshDetail:     mesh.Detail,
	}

	// 归属不在这里判：它要看全 fleet 的网段，而推送式接入下 agent 看不见
	// 别的集群（design doc 2026-08-18 §3.4）。采完之后统一走 Classify，
	// PULL 与 PUSH 共用同一份实现。
	return snap, warnings
}
