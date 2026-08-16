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

	scope, scopeWarnings := c.classifyPodIP(p.Status.PodIP, subject)
	snap.IPScope = scope.Scope
	snap.IPScopeReason = scope.Reason
	warnings = append(warnings, scopeWarnings...)

	return snap, warnings
}

// classifyPodIP 判定 Pod IP 的归属，并对与登记不符的情况产出告警。
//
// 这是 cluster.Registry 在生产路径上的调用点。它有两个作用，缺一不可：
// 把"注册表里的网段填错了"变成采集当时就能发现的事实，而不是等到求值
// 阶段以错误的集群归属表现出来；以及让分类器有一个删掉就会被测出来的
// 使用者 —— 一个没有调用点的守卫，其测试证明不了系统正确。
func (c *Collector) classifyPodIP(ip, subject string) (cluster.Classification, []snapshot.Warning) {
	if ip == "" {
		// 尚未分配 IP 的 Pod 不是异常，Phase 已经解释了它。
		return cluster.Classification{}, nil
	}

	got, err := c.registry.Classify(ip)
	if err != nil {
		return cluster.Classification{}, []snapshot.Warning{{
			Kind:    snapshot.WarningPodIPUnparsable,
			Subject: subject,
			Detail:  ip,
		}}
	}

	switch got.Scope {
	case cluster.ScopeAmbiguous:
		return got, []snapshot.Warning{{
			Kind:    snapshot.WarningPodIPAmbiguous,
			Subject: subject,
			Detail:  ip,
		}}
	case cluster.ScopeUnknown:
		return got, []snapshot.Warning{{
			Kind:    snapshot.WarningPodIPUnclassifiable,
			Subject: subject,
			Detail:  ip + " (" + string(got.Reason) + ")",
		}}
	case cluster.ScopePod:
		if got.ClusterID != c.clusterID {
			// 我们正从 c.clusterID 采集这个 Pod，而它的 IP 落在另一个集群
			// 的登记网段里。两份登记至少有一份是错的，而错的那份会让
			// 每一条涉及这个 IP 的流量都被还原成错误集群的主体。
			return got, []snapshot.Warning{{
				Kind:    snapshot.WarningPodIPOutsideCluster,
				Subject: subject,
				Detail:  ip + " falls in registered pod CIDR of " + got.ClusterID,
			}}
		}
		return got, nil
	default:
		// EXTERNAL / SERVICE / NODE：一个 Pod 的 IP 不该落在这些类别里。
		return got, []snapshot.Warning{{
			Kind:    snapshot.WarningPodIPOutsideCluster,
			Subject: subject,
			Detail:  ip + " classified as " + string(got.Scope),
		}}
	}
}
