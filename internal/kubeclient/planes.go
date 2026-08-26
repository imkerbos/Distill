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
	// Scopes 是这些策略覆盖的主体范围，用于把降级收窄到真的被覆盖的主体。
	//
	// 在这之前，集群里只要存在一条 CNP，每一条判定都会被标成 DEGRADED ——
	// 粒度粗到等于宣布这个集群完全不可信，而降级面越大，操作者越会习惯性
	// 忽略它（design doc 2026-08-25 §2）。
	Scopes []PlaneScope
	// ScopesComplete 表示这份覆盖范围是**完整**的。
	//
	// **为 false 时调用方必须整片降级**，不得只降 Scopes 里那些：范围不完整
	// 意味着有主体被覆盖而我们不知道是哪些，而漏掉一个就是把一条真的被管着
	// 的连接判成可信。Present 为 true 而 ScopesComplete 为 false 是常态，
	// 不是异常 —— 平台只解析它确定算得出来的那部分。
	ScopesComplete bool
}

// probedPlanes 是平台知道自己**不解释**的那几个策略平面。
//
// 只列到 GroupVersionResource 一级，不解析任何对象内容：平台回答的是
// "存不存在"，不是"它们放行了什么"—— 后者是第二套求值引擎（design doc §6）。
// maxPlaneObjects 是一次探测允许读入的对象数上限。
//
// 超过即放弃精确降级、退回整片降级，**不拿被截断的一半作答**：少读一条
// 就是漏掉一批被覆盖的主体，而漏掉的那些会被判成可信。
const maxPlaneObjects = 1000

var probedPlanes = []struct {
	name string
	gvr  schema.GroupVersionResource
	// scoped 表示平台会解析这一类的覆盖范围。
	//
	// 只有 Cilium 那两类：它们的 endpointSelector 是一组标签相等条件，
	// 确定算得出来。AdminNetworkPolicy 一族带优先级与 Pass 动作，覆盖范围
	// 之外还要解释生效次序，本轮不碰 —— 它存在就整片降级。
	scoped bool
	// namespaced 表示这一类是命名空间级的；否则是集群级。
	namespaced bool
}{
	{"CiliumNetworkPolicy", schema.GroupVersionResource{
		Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}, true, true},
	{"CiliumClusterwideNetworkPolicy", schema.GroupVersionResource{
		Group: "cilium.io", Version: "v2", Resource: "ciliumclusterwidenetworkpolicies"}, true, false},
	{"AdminNetworkPolicy", schema.GroupVersionResource{
		Group: "policy.networking.k8s.io", Version: "v1alpha1", Resource: "adminnetworkpolicies"}, false, false},
	{"BaselineAdminNetworkPolicy", schema.GroupVersionResource{
		Group: "policy.networking.k8s.io", Version: "v1alpha1", Resource: "baselineadminnetworkpolicies"}, false, false},
	// Calico 的私有策略平面。**它有 deny 与 order（优先级）**，与标准
	// NetworkPolicy 叠加生效 —— 与 CNP 同一类风险。
	//
	// **不解析覆盖范围**（scoped=false）：Calico 的 selector 是一个字符串
	// 表达式（`label == "value"` 语法），不是 LabelSelector，还带 tier 分层。
	// 解析它需要一个表达式解析器，而解析错会圈出错误的主体集合 —— 与
	// matchExpressions 那条同一个理由，往"算不出"倒。
	//
	// **staged 变体刻意不列**：那是影子模式，只产生指标、不真的执行。
	// 把它算作生效的第二平面，会让一个只是在预演的集群被无谓地整片降级，
	// 而降级面越大，操作者越会习惯性忽略它。
	{"CalicoGlobalNetworkPolicy", schema.GroupVersionResource{
		Group: "crd.projectcalico.org", Version: "v1", Resource: "globalnetworkpolicies"}, false, false},
	{"CalicoNetworkPolicy", schema.GroupVersionResource{
		Group: "crd.projectcalico.org", Version: "v1", Resource: "networkpolicies"}, false, true},
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

	// ScopesComplete 起点为 true，任何一处算不出就落回 false。
	// 起点为 false 会让"什么都没有"的集群也拿不到精确降级。
	out := PlaneProbe{Checked: true, ScopesComplete: true}
	for _, plane := range probedPlanes {
		gv := plane.gvr.GroupVersion().String()
		if !known[gv] {
			// 这一类的 API 组根本不存在 —— 一个确定的"没有"。
			continue
		}
		// 列全量而不是 Limit:1：要拿覆盖范围就得看每一条。上限之内一次列完，
		// 超过上限则整份范围作废（见下），不截断作答。
		list, err := dyn.Resource(plane.gvr).List(ctx, metav1.ListOptions{Limit: maxPlaneObjects + 1})
		if err != nil {
			// 组在、却列不出来（多半是没有 RBAC）。这一类的答案是"不知道"，
			// 而一个不知道就让整次探测降级 —— 不能拿其余几类的"没有"凑成
			// 一个整体的"没有"。
			out.Checked = false
			out.ScopesComplete = false
			continue
		}
		if len(list.Items) == 0 {
			continue
		}
		out.Present = true
		out.Detail = append(out.Detail, plane.name)

		if !plane.scoped {
			// 这一类平台还不会解析覆盖范围（AdminNetworkPolicy 一族）。
			// 它存在就意味着有主体被覆盖而我们说不出是哪些。
			out.ScopesComplete = false
			continue
		}
		if len(list.Items) > maxPlaneObjects {
			// 超过上限：**整份范围作废**，不拿被截断的一半作答 ——
			// 少一条就是漏掉一批被覆盖的主体。
			out.ScopesComplete = false
			continue
		}
		for _, item := range list.Items {
			scope, ok := scopeOf(item, plane.namespaced)
			if !ok {
				// 这一条的覆盖范围算不出来（matchExpressions、别的标签来源、
				// 或者 selector 写在 specs[] 里）。一条算不出，整份就不完整。
				out.ScopesComplete = false
				continue
			}
			out.Scopes = append(out.Scopes, scope)
		}
	}
	return out, nil
}
