package collect

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// adminPolicyGroup 是 ANP 一族的 API 组。
const adminPolicyGroup = "policy.networking.k8s.io"

// adminPolicyVersion 是本平台采集的版本。
//
// 钉死在 v1alpha1 而不是走 discovery 取最新：这一族的字段在版本之间会变，
// 而"按 v1alpha1 的理解去读一份 v1beta1 对象"会静默丢掉新字段 ——
// 丢掉的恰恰是我们还没学会解释的那些。版本升级要作为一次显式改动进来。
const adminPolicyVersion = "v1alpha1"

// maxAdminPolicies 是一次采集允许读入的管理面策略数上限。
//
// 超过即整类失败，**不拿被截断的一半落库**：管理面策略是有序短路求值的，
// 少一条就可能少掉那条命中的 Deny，而少掉 Deny 的方向是把一条其实被拦住
// 的连接判成放行。这一族对象在真实集群里是几十条量级，撞到这个上限
// 说明看到的不是一个正常集群。
const maxAdminPolicies = 500

// adminPolicyKinds 是采集的两类管理面策略。
var adminPolicyKinds = []struct {
	kind snapshot.AdminPolicyKind
	// apiKind 是清单里的 kind 字段值。
	apiKind string
	gvr     schema.GroupVersionResource
	// hasPriority 表示这一类带 spec.priority。
	hasPriority bool
}{
	{snapshot.AdminPolicyAdmin, "AdminNetworkPolicy", schema.GroupVersionResource{
		Group: adminPolicyGroup, Version: adminPolicyVersion, Resource: "adminnetworkpolicies"}, true},
	{snapshot.AdminPolicyBaseline, "BaselineAdminNetworkPolicy", schema.GroupVersionResource{
		Group: adminPolicyGroup, Version: adminPolicyVersion, Resource: "baselineadminnetworkpolicies"}, false},
}

// collectAdminPolicies 采集 ANP 与 BANP，保留 YAML 原文。
//
// **CRD 不存在不算失败。** 绝大多数集群没装这一族 CRD，那时 apiserver 返回
// 404，而它的含义是确定的"这里不可能有这类对象"，不是"没查着"。把它记成
// 失败会让每一个正常集群的每一次采集都带着一条永远修不掉的失败记录，
// 而一份长期非空的失败清单等于没有失败清单。
//
// 无权限、超时则照常失败：那些是真的没查出结果。
func (c *Collector) collectAdminPolicies(ctx context.Context, obs *snapshot.Observation) error {
	if c.dynamic == nil {
		// 没有动态客户端就采不了这一类。**报失败而不是静默跳过**：
		// 静默跳过之后，库里的"零条 ANP"与一个真的没有 ANP 的集群完全
		// 一样，而下游据此认为这个集群不存在管理面策略。
		return fmt.Errorf("collect: no dynamic client for admin policies in %s", c.clusterID)
	}

	var total int
	for _, k := range adminPolicyKinds {
		err := eachPage(ctx, func(ctx context.Context, opts metav1.ListOptions) (string, error) {
			list, err := c.dynamic.Resource(k.gvr).List(ctx, opts)
			if err != nil {
				return "", fmt.Errorf("list %s in %s: %w", k.gvr.Resource, c.clusterID, err)
			}
			for i := range list.Items {
				total++
				if total > maxAdminPolicies {
					return "", fmt.Errorf(
						"collect: more than %d admin policies in %s; refusing a truncated view",
						maxAdminPolicies, c.clusterID)
				}
				p, err := adminPolicyOf(c.clusterID, k.kind, k.apiKind, k.hasPriority, &list.Items[i])
				if err != nil {
					return "", err
				}
				obs.AdminPolicies = append(obs.AdminPolicies, p)
			}
			return list.GetContinue(), nil
		})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// adminPolicyOf 把一个 unstructured 对象翻成落库记录。
func adminPolicyOf(
	clusterID string,
	kind snapshot.AdminPolicyKind,
	apiKind string,
	hasPriority bool,
	obj *unstructured.Unstructured,
) (snapshot.AdminPolicy, error) {
	// List 返回的对象不带 TypeMeta，补回来：缺 apiVersion 与 kind 的清单
	// 再也无法被当作一份 Kubernetes 清单读回去。
	obj.SetAPIVersion(adminPolicyGroup + "/" + adminPolicyVersion)
	obj.SetKind(apiKind)
	// managedFields 是 apply 的簿记，与策略语义无关，体积常常超过 spec 本身。
	obj.SetManagedFields(nil)

	manifest, err := yaml.Marshal(obj.Object)
	if err != nil {
		return snapshot.AdminPolicy{}, fmt.Errorf(
			"marshal %s %s in %s: %w", apiKind, obj.GetName(), clusterID, err)
	}

	p := snapshot.AdminPolicy{
		ClusterID: clusterID,
		Kind:      kind,
		Name:      obj.GetName(),
		UID:       string(obj.GetUID()),
		Manifest:  string(manifest),
	}

	if hasPriority {
		// 读不出优先级时留 PriorityKnown=false，**不回落成 0**：0 是合法的
		// 且是最高的优先级，回落等于把一条读不懂的策略排到所有策略之前。
		if v, ok, err := unstructured.NestedInt64(obj.Object, "spec", "priority"); err == nil && ok {
			if v >= 0 && v <= int64(maxAdminPolicyPriority) {
				p.Priority = int32(v)
				p.PriorityKnown = true
			}
		}
	}
	return p, nil
}

// maxAdminPolicyPriority 是 ANP 优先级的上界（API 定义为 0–1000）。
//
// 落库前查范围而不是直接转换：int64 转 int32 会静默截断，
// 而一个截断出来的数看上去和一个真的优先级一模一样。
const maxAdminPolicyPriority = 1000
