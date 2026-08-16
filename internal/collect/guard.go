package collect

import (
	"context"
	"fmt"
	"slices"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// writeVerbs 是采集身份绝不该拥有的动词。
var writeVerbs = []string{"create", "update", "patch", "delete", "deletecollection"}

// policyResource 是一类网络策略资源。
type policyResource struct {
	group    string
	resource string
}

// policyResources 是三类策略对象。
//
// Cilium 的两类一并检查：CLAUDE.md 说的是"日常 Kubernetes 策略写权限"，
// 而在装了 Cilium 的集群上，CCNP 与 CNP 同样是能阻断生产流量的策略平面。
// 只查标准 NetworkPolicy 会让一个持有 CCNP 写权限的凭据顺利通过自检。
var policyResources = []policyResource{
	{"networking.k8s.io", "networkpolicies"},
	{"cilium.io", "ciliumnetworkpolicies"},
	{"cilium.io", "ciliumclusterwidenetworkpolicies"},
}

// AssertReadOnly 自证当前凭据不具备任何网络策略的写权限。
//
// CLAUDE.md 把"平台持有日常 Kubernetes 策略写权限"定为设计错误而非权限
// 配置问题。进程隔离只保证主服务没有 kubeconfig；这条自检把采集器自身的
// 只读性从一条部署约定变成每次启动都被验证的运行时事实。
//
// SelfSubjectAccessReview 本身是一次 create 调用，但它创建的是一个不落盘
// 的鉴权问询对象，不改变集群任何状态 —— 这是 Kubernetes 提供的自省接口，
// 不是本条约束所说的写操作。
//
// **无法完成自检时一律拒绝启动。** 连"我有没有写权限"都问不出来的时候，
// 假定没有是这条守卫最没用的失败方向。
func AssertReadOnly(ctx context.Context, client kubernetes.Interface) error {
	var granted []string

	for _, res := range policyResources {
		for _, verb := range writeVerbs {
			review := &authv1.SelfSubjectAccessReview{
				Spec: authv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authv1.ResourceAttributes{
						Group:    res.group,
						Resource: res.resource,
						Verb:     verb,
					},
				},
			}

			got, err := client.AuthorizationV1().SelfSubjectAccessReviews().
				Create(ctx, review, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf(
					"collect: cannot verify read-only access for %s %s.%s: %w",
					verb, res.resource, res.group, err)
			}
			if got.Status.Allowed {
				granted = append(granted, fmt.Sprintf("%s %s.%s", verb, res.resource, res.group))
			}
		}
	}

	if len(granted) > 0 {
		slices.Sort(granted)
		return fmt.Errorf(
			"collect: credential grants network policy write access (%s); "+
				"the platform must never hold day-to-day policy write permission",
			strings.Join(granted, ", "))
	}
	return nil
}
