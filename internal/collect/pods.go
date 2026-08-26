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
		ClusterID:         c.clusterID,
		Namespace:         p.Namespace,
		Name:              p.Name,
		UID:               string(p.UID),
		Phase:             string(p.Status.Phase),
		IP:                p.Status.PodIP,
		ExtraIPs:          extraPodIPs(p),
		Labels:            p.Labels,
		HostNetwork:       p.Spec.HostNetwork,
		NodeName:          p.Spec.NodeName,
		ServiceAccount:    p.Spec.ServiceAccountName,
		OwnerKind:         owner.Kind,
		OwnerName:         owner.Name,
		WorkloadKind:      workload.Kind,
		WorkloadName:      workload.Name,
		InMesh:            mesh.InMesh,
		MeshSource:        mesh.Source,
		MeshDetail:        mesh.Detail,
		ScrapeAnnotations: scrapeAnnotationsOf(p.Annotations),
	}

	// 归属不在这里判：它要看全 fleet 的网段，而推送式接入下 agent 看不见
	// 别的集群（design doc 2026-08-18 §3.4）。采完之后统一走 Classify，
	// PULL 与 PUSH 共用同一份实现。
	return snap, warnings
}

// ScrapeAnnotationKeys 是允许被采集的 metrics 抓取注解键。
//
// **白名单，不是过滤器。** 整批采集 annotations 会把
// kubectl.kubernetes.io/last-applied-configuration 一起抄进来 —— 那是整份
// manifest，体积上是 labels 的几十倍，内容上可能带着 env 里的口令与内网地址，
// 而这个库会被导出到事实层长期留存（design doc 2026-08-18 §5，V4 spec §9.9）。
//
// path 一并采：NetworkPolicy 不管路径，但「这条 Baseline 当初凭什么生成」
// 要在事后答得出来，而那时这个 Pod 可能已经不在了。
//
// 白名单放在 Go 侧而不是查询里：写进 SQL 意味着改一次要同时改采集与读取
// 两处，而漏改的那一处不会报错。
var ScrapeAnnotationKeys = []string{
	"prometheus.io/scrape",
	"prometheus.io/port",
	"prometheus.io/path",
}

// scrapeAnnotationsOf 从 Pod 的注解里挑出白名单那几个。
//
// 一个都没有时返回 nil 而不是空 map：**不得凭空补一个默认端口**。一条放行到
// 猜出来的端口的规则，看起来齐备、实际什么都没放行，而症状要到监控静默中断
// 时才出现（derive_infra.go 的注释）。
func scrapeAnnotationsOf(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	var out map[string]string
	for _, k := range ScrapeAnnotationKeys {
		v, ok := annotations[k]
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(ScrapeAnnotationKeys))
		}
		out[k] = v
	}
	return out
}

// extraPodIPs 取出 status.podIPs 里除主地址之外的那些。
//
// **双栈 Pod 有两个地址**，而 status.podIP 只是 status.podIPs 的第一项。
// 漏掉第二个的后果是走它的连接解不出主体、判 UNKNOWN，覆盖它的规则于是
// 缺席 —— 下发 default-deny 之后那条连接会被拦断。
//
// 按值比对主地址而不是按下标跳过第一项：Kubernetes 保证两者一致，但一份
// 不一致的 status（改过的、或未来版本的）不该让主地址被当成"额外地址"重复
// 记一遍。重复本身无害（区间按地址建，同一个地址两条会被折叠），但那时
// 快照里会出现一个与 IP 列相同的 ExtraIP，读起来像数据坏了。
func extraPodIPs(p *corev1.Pod) []snapshot.PodAddress {
	if len(p.Status.PodIPs) <= 1 {
		return nil
	}
	out := make([]snapshot.PodAddress, 0, len(p.Status.PodIPs)-1)
	for _, addr := range p.Status.PodIPs {
		if addr.IP == "" || addr.IP == p.Status.PodIP {
			continue
		}
		out = append(out, snapshot.PodAddress{IP: addr.IP})
	}
	return out
}
