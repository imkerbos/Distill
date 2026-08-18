// Package collect 从单个 Kubernetes 集群采集资产的时间点快照。
//
// 这是平台唯一使用 client-go 的生产代码路径，且**只读**：采集器不写集群，
// 启动时以 SelfSubjectAccessReview 自证不具备 NetworkPolicy 的写权限
// （见 guard.go）。平台产出策略、Git 存储策略、Config Sync 应用策略，
// 采集这一端在架构上不该有任何写能力。
package collect

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// listPageSize 限制单次 List 的返回量。
//
// 不分页会让一个大集群的 Pod 列表一次性进内存，而采集器的内存上限
// 与集群规模无关 —— 那是别人的集群，不是我们能约束的输入。
const listPageSize = 500

// Collector 从单个集群采集资产全量快照。
type Collector struct {
	clusterID string
	client    kubernetes.Interface
	now       func() time.Time
}

// New 构造针对指定集群的采集器。
//
// **不再接收 fleet 的网段登记**（design doc 2026-08-18 §3.4）：归属判定要看
// 全 fleet 才答得出「这个 IP 落在别的集群网段里」，而推送式接入下采集器
// 跑在被管集群内，给它 fleet 登记等于把整个 fleet 的拓扑发出去。判定挪到
// 采集之后的 Classify，PULL 与 PUSH 共用同一份实现。
//
// now 可注入以保证测试确定性。
func New(clusterID string, client kubernetes.Interface, now func() time.Time) *Collector {
	if now == nil {
		now = time.Now
	}
	return &Collector{clusterID: clusterID, client: client, now: now}
}

// resourceCollector 是一类资源的采集动作。
type resourceCollector struct {
	kind    snapshot.ResourceKind
	collect func(context.Context, *snapshot.Observation) error
}

// Collect 采集一次全量快照。
//
// 单类资源失败不中断整轮：一次采不到 NetworkPolicy 的运行，
// 其 Pod 与 Service 数据仍然是真的。中断会让一个权限缺口
// 表现成"这个集群什么都采不到"，掩盖真正的成因。
func (c *Collector) Collect(ctx context.Context, runID string) (snapshot.Run, error) {
	if runID == "" {
		return snapshot.Run{}, errors.New("collect: empty run id; a run that cannot be joined is not a run")
	}

	startedAt := c.now()
	obs := snapshot.Observation{
		ClusterID: c.clusterID,
		RunID:     runID,
		// 与 StartedAt 取同一个值而非各调一次 now()：这批记录必须共用
		// 一个观测时刻，两次取值会让同一次运行的行带上互不相同的时间，
		// 而"这两条记录是不是同一次看到的"正是历史回放的判据。
		ObservedAt: startedAt,
	}

	// namespaces 与 replicaSets 先采：前者提供 mesh 判定的回退依据，
	// 后者提供 ownerRef 链的中间跳。两者失败时 Pod 仍然照采，
	// 只是相应字段退化 —— 那比整轮放弃更接近事实。
	nsLabels := map[string]map[string]string{}
	rsOwners := map[string]ownerRef{}

	steps := []resourceCollector{
		{snapshot.ResourceNamespace, func(ctx context.Context, o *snapshot.Observation) error {
			return c.collectNamespaces(ctx, o, nsLabels)
		}},
		{snapshot.ResourceReplicaSet, func(ctx context.Context, _ *snapshot.Observation) error {
			return c.collectReplicaSetOwners(ctx, rsOwners)
		}},
		{snapshot.ResourcePod, func(ctx context.Context, o *snapshot.Observation) error {
			return c.collectPods(ctx, o, rsOwners)
		}},
		{snapshot.ResourceNode, c.collectNodes},
		{snapshot.ResourceService, c.collectServices},
		{snapshot.ResourceEndpointSlice, c.collectEndpointSlices},
		{snapshot.ResourceNetworkPolicy, c.collectNetworkPolicies},
		{snapshot.ResourceIngress, c.collectIngresses},
	}

	attempted := make([]snapshot.ResourceKind, 0, len(steps))
	var failures []snapshot.Failure
	for _, s := range steps {
		attempted = append(attempted, s.kind)
		if err := s.collect(ctx, &obs); err != nil {
			failures = append(failures, snapshot.Failure{
				Resource: s.kind,
				Reason:   classifyFailure(err),
				Detail:   err.Error(),
			})
		}
	}

	// Service 的后端非空校验放在全部资源采完之后：它要同时看见
	// Service 与 EndpointSlice，任一类失败时这条校验没有依据，必须跳过，
	// 否则会把"没采到 EndpointSlice"报成"所有 Service 都没有后端"。
	if !failed(failures, snapshot.ResourceService) && !failed(failures, snapshot.ResourceEndpointSlice) {
		obs.Warnings = append(obs.Warnings, servicesWithoutEndpoints(obs.Services, obs.Endpoints)...)
	}

	return snapshot.Run{
		Observation: obs,
		Failures:    failures,
		Status:      snapshot.DeriveRunStatus(attempted, failures),
		StartedAt:   startedAt,
		FinishedAt:  c.now(),
	}, nil
}

// failed 报告某类资源是否在本次运行中失败。
func failed(failures []snapshot.Failure, kind snapshot.ResourceKind) bool {
	for _, f := range failures {
		if f.Resource == kind {
			return true
		}
	}
	return false
}

// classifyFailure 把 apiserver 返回的错误归入封闭枚举。
//
// FORBIDDEN 必须单独可辨：它是唯一一种"集群是好的、是我们没被授权"的
// 失败，处置是改 RBAC 而非重试，而重试恰恰是它最常被误配的处置方式。
func classifyFailure(err error) snapshot.FailureReason {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return snapshot.FailureForbidden
	case apierrors.IsNotFound(err):
		return snapshot.FailureNotFound
	case apierrors.IsTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return snapshot.FailureTimeout
	case apierrors.IsServiceUnavailable(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err):
		return snapshot.FailureUnavailable
	default:
		return snapshot.FailureOther
	}
}

// eachPage 按分页拉完一类资源。
//
// fn 自行累积本页条目并返回 continue token；返回空串表示已到末页。
func eachPage(ctx context.Context, fn func(context.Context, metav1.ListOptions) (string, error)) error {
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		cont, err := fn(ctx, opts)
		if err != nil {
			return err
		}
		if cont == "" {
			return nil
		}
		opts.Continue = cont
	}
}

// collectNamespaces 采集全部命名空间，并把标签写进 nsLabels 供 Pod 判定使用。
func (c *Collector) collectNamespaces(ctx context.Context, obs *snapshot.Observation, nsLabels map[string]map[string]string) error {
	return eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
		list, err := c.client.CoreV1().Namespaces().List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list namespaces in %s: %w", c.clusterID, err)
		}
		for i := range list.Items {
			ns := &list.Items[i]
			nsLabels[ns.Name] = ns.Labels
			mesh := cluster.DetectNamespaceMesh(ns.Labels)
			obs.Namespaces = append(obs.Namespaces, snapshot.Namespace{
				ClusterID:  c.clusterID,
				Name:       ns.Name,
				Labels:     ns.Labels,
				InMesh:     mesh.InMesh,
				MeshSource: mesh.Source,
				MeshDetail: mesh.Detail,
			})
		}
		return list.Continue, nil
	})
}

// collectReplicaSetOwners 只取 ReplicaSet 的控制器引用，不落库。
//
// ReplicaSet 本身不是资产，采它唯一的用途是把 Pod 解析到 Deployment。
// 落库会让每次发布产生一批很快就无人引用的记录。
func (c *Collector) collectReplicaSetOwners(ctx context.Context, rsOwners map[string]ownerRef) error {
	return eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
		list, err := c.client.AppsV1().ReplicaSets(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("list replicasets in %s: %w", c.clusterID, err)
		}
		for i := range list.Items {
			rs := &list.Items[i]
			rsOwners[rs.Namespace+"/"+rs.Name] = controllerOf(rs.OwnerReferences)
		}
		return list.Continue, nil
	})
}

// controllerOf 返回 ownerRef 列表中被标为 controller 的那一个。
//
// 取 controller 而非第一个：一个对象可以有多个 owner，但只有一个控制它。
// 取第一个会在存在附加 owner 时把主体解析成一个不相干的对象。
func controllerOf(refs []metav1.OwnerReference) ownerRef {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return ownerRef{Kind: refs[i].Kind, Name: refs[i].Name}
		}
	}
	return ownerRef{}
}
