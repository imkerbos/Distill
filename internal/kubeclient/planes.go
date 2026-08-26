package kubeclient

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// PlaneProbe 是一次「除标准 NetworkPolicy 之外还有没有别的策略平面」的探测结果
// （design doc 2026-08-25 §2.3）。
//
// 三个字段各自回答一件事，不合并：Present 说"有"，Checked 说"查过了"，
// Detail 说"是哪一类"。**Present 为 false 且 Checked 为 false 时，含义是
// "不知道"，不是"没有"** —— 调用方据此写 UNKNOWN。
type PlaneProbe struct {
	// Present 表示确认存在至少一个其它平面的策略对象。
	Present bool
	// Checked 表示这次探测真的完成了（每一个平面都得到了确定答案）。
	Checked bool
	// Detail 是探测到的平面种类，用于展示；不含地址、不含对象名。
	Detail []string
}

// probedPlanes 是平台知道自己**不解释**的那几个策略平面。
//
// 只列到 GroupVersionResource 一级，不解析任何对象内容：平台回答的是
// "存不存在"，不是"它们放行了什么"—— 后者是第二套求值引擎（design doc §6）。
var probedPlanes = []struct {
	name string
	gvr  schema.GroupVersionResource
}{
	{"CiliumNetworkPolicy", schema.GroupVersionResource{
		Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}},
	{"CiliumClusterwideNetworkPolicy", schema.GroupVersionResource{
		Group: "cilium.io", Version: "v2", Resource: "ciliumclusterwidenetworkpolicies"}},
	{"AdminNetworkPolicy", schema.GroupVersionResource{
		Group: "policy.networking.k8s.io", Version: "v1alpha1", Resource: "adminnetworkpolicies"}},
	{"BaselineAdminNetworkPolicy", schema.GroupVersionResource{
		Group: "policy.networking.k8s.io", Version: "v1alpha1", Resource: "baselineadminnetworkpolicies"}},
}

// ProbePlanes 探测目标集群里有没有平台不解释的其它策略平面。
//
// **失败一律报"没查成"，不报"没有"**：无权限、发现接口不可用、超时，
// 三种情况在返回值上都是 Checked=false。一次查不动的探测必须表现为
// "我不知道"，否则它与"确认不存在"在下游长得一模一样，而后者会让平台
// 以满置信度回答每一条判定（design doc §2.3）。
//
// 只读：ServerGroups 与 List，不建任何对象。
func ProbePlanes(ctx context.Context, kubeconfig []byte) (PlaneProbe, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return PlaneProbe{}, fmt.Errorf("kubeclient: parse kubeconfig: %w", err)
	}
	cfg.UserAgent = userAgent
	cfg.Timeout = requestTimeout
	cfg.Dial = guardedDial
	return probePlanes(ctx, cfg)
}

// probePlanes 在给定配置上做探测，便于测试注入。
func probePlanes(ctx context.Context, cfg *rest.Config) (PlaneProbe, error) {
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return PlaneProbe{}, fmt.Errorf("kubeclient: build discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return PlaneProbe{}, fmt.Errorf("kubeclient: build dynamic client: %w", err)
	}

	groups, err := disco.ServerGroups()
	if err != nil {
		// 发现接口都问不出来，后面的 List 更不可能可信。
		//
		// **这里刻意把错误吞掉、返回"没查成"，而不是上抛**：探测失败不该
		// 让整次采集失败 —— 资产本身仍然有价值。失败的表达方式是
		// Checked=false，下游据此写 UNKNOWN 并降级判定，那正是要的
		// （design doc 2026-08-25 §2.3）。上抛反而会让调用方在"要不要因为
		// 探测失败而丢掉这一轮采集"上做一个它不该做的决定。
		return PlaneProbe{Checked: false}, nil //nolint:nilerr // 见上：失败即"没查成"，不是错误。
	}
	known := map[string]bool{}
	for _, g := range groups.Groups {
		for _, v := range g.Versions {
			known[v.GroupVersion] = true
		}
	}

	out := PlaneProbe{Checked: true}
	for _, plane := range probedPlanes {
		gv := plane.gvr.GroupVersion().String()
		if !known[gv] {
			// 这一类的 API 组根本不存在 —— 一个确定的"没有"。
			continue
		}
		list, err := dyn.Resource(plane.gvr).List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			// 组在、却列不出来（多半是没有 RBAC）。这一类的答案是"不知道"，
			// 而一个不知道就让整次探测降级 —— 不能拿其余几类的"没有"凑成
			// 一个整体的"没有"。
			out.Checked = false
			continue
		}
		if len(list.Items) > 0 {
			out.Present = true
			out.Detail = append(out.Detail, plane.name)
		}
	}
	return out, nil
}
