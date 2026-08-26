package policygen

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// nsNameLabel 是 Kubernetes 自动给每个 namespace 打上的名称标签。
const nsNameLabel = "kubernetes.io/metadata.name"

// Input 是一次生成所需的全部输入。
type Input struct {
	// ClusterID 是生成策略的目标集群。
	ClusterID string
	// Assets 是该集群的资产快照，Baseline 推导的依据。
	Assets snapshot.Assets
	// Namespaces 是该集群的命名空间快照。
	Namespaces []replay.NamespaceRef
	// Pods 是该集群的 Pod 快照，候选策略的生成单位来自它而非流量。
	//
	// 按流量生成会让 mesh 内（流量全 DEGRADED）与被写坏策略挡住（流量全 UNKNOWN）
	// 的 workload 悄悄从候选集里消失，连带绕过 Baseline 的无条件注入 ——
	// 一个不存在的策略不会缺任何东西，缺失也就报不出来。
	Pods []replay.PodRef
	// Observations 是带判定结果的观测流量。
	Observations []Observation
	// UnassessedBaselines 是依据资源这次采集没有拿回来的那几类。
	//
	// 传给 baseline.Derive，让它不把"我们没看过"判成"不适用"：资产里
	// 两者长得一模一样，而误判的方向是让一次采集失败变成一次放行
	// （design doc 2026-08-18-baseline-applicability §3）。
	// 依据齐备的数据源（fixture）传 nil。
	UnassessedBaselines []baseline.Kind
	// Imports 是人工导入、要补进候选集的策略（registry.RoleCandidateAddition）。
	//
	// **它补的是观测看不见的东西**：月结批处理、季度对账、只在故障时走的灾备
	// 链路，不在窗口里就学不出规则，而 dry-run 也报不出来（它只评估见过的
	// 连接）。这是操作者补上那条规则的入口。
	Imports []ImportedPolicy
	// ManagedSystemNamespaces 是操作者明示"由平台管"的系统命名空间。
	//
	// 默认平台不为 kube-system 一类生成候选策略（systemNamespaces）：那份
	// default-deny 一旦下发，全集群的 DNS 就可能断。要纳入必须显式列出来，
	// 而那是一次有记录的人工判断。
	ManagedSystemNamespaces []string
}

// Result 是一次生成的全部产物。
//
// 四块必须一起返回：只报 Policies 而不报 MissingBaselines、Ungeneratable
// 与 ExcludedWorkloads，等于宣称覆盖完整。Ungeneratable 报的是"某条流量
// 表达不了"，ExcludedWorkloads 报的是更前一步的缺口——"这个 workload
// 从未进入候选集，因此不会出现在任何一条流量的判定里"。少了后者，一个
// hostNetwork 或无标签的 Pod 会以"0 不可生成"的面貌被系统悄悄吞掉。
type Result struct {
	// Policies 是候选策略，按 (namespace, workload) 确定排序。
	Policies []CandidatePolicy `json:"policies"`
	// MissingBaselines 是本次涉及的 namespace 中尚未齐备的 Baseline 类型。
	MissingBaselines []MissingBaseline `json:"missingBaselines"`
	// NotApplicableBaselines 是那些在该 namespace 里**没有推导对象**的类型。
	//
	// 与 MissingBaselines 互斥、并列返回。报出来而不是静默丢弃：一份空缺失
	// 与一次根本没做的校验必须区分得开 —— 屏幕上要读得出「batch 的 LB：
	// 看过了，这个 namespace 没有暴露面」，而不是那一行凭空不见
	// （design doc 2026-08-18-baseline-applicability §5）。
	NotApplicableBaselines []MissingBaseline `json:"notApplicableBaselines"`
	// Ungeneratable 是无法表达为规则的流量。
	Ungeneratable []UngeneratableItem `json:"ungeneratable"`
	// ExcludedWorkloads 是从未进入候选策略花名册的 Pod，按 (namespace, pod) 确定排序。
	ExcludedWorkloads []ExcludedWorkload `json:"excludedWorkloads"`
	// ExcludedNamespaces 是**整体**未进候选集的命名空间。
	//
	// 与 ExcludedWorkloads 分列：那一栏说的是"这个 Pod 表达不成 selector"，
	// 这一栏说的是"这一整片平台默认不碰"。前者要去修 Pod 的标签，后者是一个
	// 有意的保护 —— 处置完全不同。
	ExcludedNamespaces []ExcludedNamespace `json:"excludedNamespaces"`
	// UnattachedImports 是挂不到任何主体上、因而没有进候选集的导入。
	//
	// **必须与候选集一起返回**：一条导入进来了却没出现在候选集里，操作者会
	// 以为它生效了 —— 而它恰恰是用来补那条平台看不见的连接的，"以为补上了"
	// 比"知道没补上"危险得多。
	UnattachedImports []UnattachedImport `json:"unattachedImports"`
}

// MissingBaseline 是一个 namespace 缺失的 Baseline 类型。
type MissingBaseline struct {
	// Namespace 是缺失所在的命名空间。
	Namespace string `json:"namespace"`
	// Kinds 是缺失的类型，按登记顺序。
	Kinds []baseline.Kind `json:"kinds"`
}

// Generate 从流量证据与快照生成候选策略集。纯函数。
//
// 生成范围恒为整个集群，不接受 namespace 过滤：dry-run 预测必须跑在
// 完整策略集上，一份被裁剪过的策略集配全量流量会让目的地在其他
// namespace 的流量落到 ALLOW，凭空造出敞口告警（spec §5）。按 namespace
// 看只是展示需求，由消费方裁剪产物完成。
func Generate(in Input) Result {
	// 归属键的赢家先定下来：名册与流量两条路径必须用同一份判定，各算
	// 各的会让「哪个 Pod 进了候选集」与「哪条流量学得进规则」互相矛盾。
	winners := resolveWinningKeys(in)

	counts := map[aggKey]int{}
	var bad []UngeneratableItem

	for _, o := range in.Observations {
		items, gaps := classify(o, in.ClusterID, winners)
		for _, it := range items {
			counts[it.key]++
		}
		// 不可生成项按 flow 收集，不受 namespace 过滤影响：一条表达不了的
		// 流量在哪个 namespace 视图下都同样表达不了，按视图裁剪会让这个
		// 缺口随筛选条件时隐时现。
		bad = append(bad, gaps...)
	}

	// 候选策略的生成单位是 Pod 名册，不是流量：只按 counts 里出现过的
	// workload 生成，会让 mesh 内（流量全 DEGRADED）与被写坏策略挡住
	// （流量全 UNKNOWN）的 workload 连同它们的强制 Baseline 一起从候选集
	// 里消失——这两类恰恰是最需要被看见的："平台学不出它的流量"，不是
	// "它没有流量"。
	//
	// 排除条件与 subjectOf 对齐：hostNetwork Pod 选不中，没有可识别
	// workload 标签的 Pod 无法用 podSelector 表达，两者都不能进名册，
	// 否则会生成一条谁都匹配不到、或者选中了不该选对象的幽灵策略。
	//
	// 被排除的 Pod 必须单独记下来（ExcludedWorkloads）：它们从未进入
	// 名册，因此永远不会出现在任何一条流量的判定里，Ungeneratable 那条
	// 报不出这个缺口——一个不存在的候选策略不会"缺"任何东西。
	workloads := map[subject]bool{}
	var excluded []ExcludedWorkload
	// 系统命名空间默认整片不生成（systemNamespaces）。**排除只影响生成，
	// 不影响判定**：这些 Pod 照常参与流量归属与回放，否则对账与 dry-run
	// 会跟着一起错。
	gate := namespaceGate(in.ManagedSystemNamespaces)
	excludedNS := map[string]ExcludedNamespace{}
	for _, p := range in.Pods {
		if p.ClusterID != in.ClusterID {
			continue
		}
		if ex, blocked := gate(p.Namespace); blocked {
			excludedNS[p.Namespace] = ex
		}
	}
	for _, p := range in.Pods {
		if p.ClusterID != in.ClusterID {
			continue
		}
		if _, blocked := excludedNS[p.Namespace]; blocked {
			// 整片排除，连 Baseline 也不注入：注入了就意味着要下发，
			// 而这道保护存在的理由正是"这一片不下发"。
			continue
		}
		if replay.IsUnmanaged(p) {
			excluded = append(excluded, ExcludedWorkload{
				Namespace: p.Namespace, Pod: p.Name, Labels: cloneLabels(p.Labels),
				Reason: ExclusionHostNetwork,
			})
			continue
		}
		key, wl, ok := resolveWorkloadLabel(p.Labels)
		if !ok {
			excluded = append(excluded, ExcludedWorkload{
				Namespace: p.Namespace, Pod: p.Name, Labels: cloneLabels(p.Labels),
				Reason: ExclusionNoWorkloadLabel,
			})
			continue
		}
		// 同名 workload 挂在不同标签键上时只留赢家（resolveWinningKeys），
		// 输家在这里点名报出去：赢家的 podSelector 用的是赢家的键，选不中
		// 这个 Pod，所以它确实没有候选策略。报一条"它被排除了"，好过发一条
		// 名字相同、却谁都选不中的第二份策略——后者不报错，只是永远不生效。
		if win, seen := winners[nsWorkload{namespace: p.Namespace, workload: wl}]; seen && win != key {
			excluded = append(excluded, ExcludedWorkload{
				Namespace: p.Namespace, Pod: p.Name, Labels: cloneLabels(p.Labels),
				Reason: ExclusionLabelKeyConflict,
			})
			continue
		}
		workloads[subject{namespace: p.Namespace, workload: wl, labelKey: key}] = true
	}
	sort.Slice(excluded, func(i, j int) bool {
		if excluded[i].Namespace != excluded[j].Namespace {
			return excluded[i].Namespace < excluded[j].Namespace
		}
		return excluded[i].Pod < excluded[j].Pod
	})

	byWorkload := map[subject][]Rule{}
	for k, n := range counts {
		s := subject{namespace: k.SubjectNS, workload: k.Subject, labelKey: k.SubjectKey}
		// **整片排除的命名空间在这里也要挡住。**
		//
		// 下面那句 workloads[s] = true 会把主体加回名册，因此只在 Pod 名册
		// 那一处跳过是不够的：kube-system 里有流量的 workload 会从这条路
		// 绕回候选集，而排除清单照样报着它 —— 一个"报了却没生效"的保护，
		// 比没有保护更危险（真集群实测发现，2026-08-26）。
		if _, blocked := excludedNS[k.SubjectNS]; blocked {
			continue
		}
		// 名册之外仍学到规则理论上不会发生——subjectOf 已经把 hostNetwork
		// 与无标签 Pod 挡在外面——但稳妥起见仍然按学习结果建一条策略，
		// 而不是静默丢弃这批规则。
		workloads[s] = true
		byWorkload[s] = append(byWorkload[s], learnedRule(k, n))
	}

	// Baseline 无条件追加，永不参与去重、永不被学习结果覆盖（spec §7.1）。
	// 即使某个 namespace 一条学习规则都没有，只要它有 workload 就得有 Baseline。
	//
	// 每个 namespace 只 Derive 一次：它是纯函数，算两遍不会算出不同结果，
	// 但两个调用点各自维护一份会互相漂移——比如一处加了过滤条件、另一处
	// 忘了同步。规则与 Missing() 从同一个 Set 取，保证两者恒指同一份推导。
	nsWithWorkload := map[string]bool{}
	for s := range workloads {
		nsWithWorkload[s.namespace] = true
	}
	baselineByNS := map[string][]Rule{}
	baselineSetByNS := map[string]baseline.Set{}
	for ns := range nsWithWorkload {
		set := baseline.Derive(in.Assets, ns, in.UnassessedBaselines)
		baselineSetByNS[ns] = set
		for _, br := range set.Rules {
			baselineByNS[ns] = append(baselineByNS[ns], baselineRule(br))
		}
	}

	// 导入并进名册**之后**：一条挂到集群里并不存在的 workload 上的导入，
	// 会生成一条选不中任何 Pod 的幽灵策略 —— 它不报错，只是永远不生效，
	// 而操作者以为自己补上了那条放行。因此要拿名册核对，核不上就报出来。
	imported, unattached := importedRules(in.Imports, workloads)

	res := Result{
		Ungeneratable: dedupeGaps(bad), ExcludedWorkloads: excluded,
		UnattachedImports:  unattached,
		ExcludedNamespaces: sortedExcludedNamespaces(excludedNS),
	}
	for s := range workloads {
		rules := append([]Rule{}, byWorkload[s]...)
		rules = append(rules, baselineByNS[s.namespace]...)
		rules = append(rules, imported[s]...)
		sortRules(rules)
		res.Policies = append(res.Policies, CandidatePolicy{
			// 生成恒为 workload 粒度 —— 那是最细的一层，也是人工确认挂靠的
			// 那一层。namespace 粒度由 AtNamespaceGranularity 折叠出来，
			// 不是第二次生成。
			Granularity: GranularityWorkload,
			Cluster:     in.ClusterID, Namespace: s.namespace, Workload: s.workload,
			WorkloadLabelKey: s.labelKey, Rules: rules,
		})
	}
	// 比较到 WorkloadLabelKey 为止：resolveWinningKeys 保证 (namespace,
	// workload) 唯一，这一项理论上永远比不到。写出来是因为 sort.Slice
	// 不稳定、上面的 res.Policies 又是 range map 建起来的——排序器一旦
	// 打平，输出顺序就退化成 map 遍历顺序，而整个指纹机制挂在"同一份
	// 输入两次生成逐字节相同"上。让确定性依赖另一个函数的不变量，是把
	// 一次未来的重构变成一次静默的指纹失效。
	sort.Slice(res.Policies, func(i, j int) bool {
		a, b := res.Policies[i], res.Policies[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		return a.WorkloadLabelKey < b.WorkloadLabelKey
	})

	for ns := range nsWithWorkload {
		set := baselineSetByNS[ns]
		if missing := set.Missing(); len(missing) > 0 {
			res.MissingBaselines = append(res.MissingBaselines,
				MissingBaseline{Namespace: ns, Kinds: missing})
		}
		if na := set.NotApplicable; len(na) > 0 {
			res.NotApplicableBaselines = append(res.NotApplicableBaselines,
				MissingBaseline{Namespace: ns, Kinds: na})
		}
	}
	sort.Slice(res.MissingBaselines, func(i, j int) bool {
		return res.MissingBaselines[i].Namespace < res.MissingBaselines[j].Namespace
	})
	sort.Slice(res.NotApplicableBaselines, func(i, j int) bool {
		return res.NotApplicableBaselines[i].Namespace < res.NotApplicableBaselines[j].Namespace
	})

	return res
}

// learnedRule 把一个聚合键变成一条学习规则。
//
// Enabled 的判定是纯函数、无参数、无阈值：只有身份可信、集群内、当前
// 放行、且端口不在风险清单里的规则才默认启用。其余全部生成、可见、
// 待人工确认 —— 学是学，推荐是推荐。
func learnedRule(k aggKey, flowCount int) Rule {
	rp, risky := risk.Lookup(k.Port)
	r := Rule{
		Origin: OriginLearned, Evidence: k.Evidence, Direction: k.Direction,
		FlowCount: flowCount,
		Enabled:   k.Evidence == EvidenceTrustedAllow && !risky,
	}
	if risky {
		copied := rp
		r.Risk = &copied
	}

	proto := corev1.Protocol(k.Protocol)
	port := intstr.FromInt32(k.Port)
	ports := []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &port}}
	peer := peerSpec(k)

	if k.Direction == replay.DirectionEgress {
		r.Egress = &networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{peer}, Ports: ports,
		}
	} else {
		r.Ingress = &networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{peer}, Ports: ports,
		}
	}
	r.describe()
	return r
}

// peerSpec 把聚合键里的对端表达成 NetworkPolicy peer。
//
// podSelector 必须用 k.PeerKey——对端实际命中的标签键——而不是固定的
// app：一条从 k8s-app 归属出来的 coredns 对端若被拼成 {app: kube-dns}，
// 生成的是一条集群里没有任何 Pod 会命中的幽灵 selector。
func peerSpec(k aggKey) networkingv1.NetworkPolicyPeer {
	if k.PeerCIDR != "" {
		return networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: k.PeerCIDR},
		}
	}
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{nsNameLabel: k.PeerNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{k.PeerKey: k.PeerWorkload},
		},
	}
}

// cloneLabels 拷贝一份 Pod 标签，供 ExcludedWorkload 使用。
//
// 不直接引用 in.Pods 里的底层 map：ExcludedWorkload 要在生成之外存活
// 展示，若与快照共享同一份 map，调用方对快照的任何后续修改都会
// 悄悄改写已经返回给界面的排查证据。
func cloneLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// baselineRule 把一条 Baseline 包装成候选策略里的规则。
//
// Baseline 恒为 Enabled：它们是必备项，不是建议项。
func baselineRule(br baseline.Rule) Rule {
	kind := br.Kind
	r := Rule{
		Origin: OriginBaseline, Baseline: &kind, Derivations: br.Derivations,
		Direction: br.Direction, Enabled: true,
		Ingress: br.Ingress, Egress: br.Egress,
	}
	r.describe()
	return r
}

// sortRules 给规则定序：Baseline 在前，其后按方向、证据、端口、协议、对端。
//
// 确定排序不是美观问题：同一份输入两次生成必须逐字节相同，否则产物
// diff 全是噪声，review 随之失效。比较键必须覆盖两条规则之间可能不同
// 的每一个字段——聚合键（aggKey）本身就是 map 键，两条不同的学习规则
// 保证在 Direction/Evidence/Port/Protocol/Peer 中至少有一处不同；若比较
// 到端口就停手，这些规则会在同一端口打平，排序结果退化成 map 遍历顺序，
// 也就是随机顺序。
func sortRules(rules []Rule) {
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if (a.Origin == OriginBaseline) != (b.Origin == OriginBaseline) {
			return a.Origin == OriginBaseline
		}
		if a.Direction != b.Direction {
			return a.Direction < b.Direction
		}
		if a.Evidence != b.Evidence {
			return a.Evidence < b.Evidence
		}
		if pa, pb := rulePort(a), rulePort(b); pa != pb {
			return pa < pb
		}
		if pa, pb := ruleProtocol(a), ruleProtocol(b); pa != pb {
			return pa < pb
		}
		nsA, wlA, cidrA := rulePeer(a)
		nsB, wlB, cidrB := rulePeer(b)
		if nsA != nsB {
			return nsA < nsB
		}
		if wlA != wlB {
			return wlA < wlB
		}
		return cidrA < cidrB
	})
}

// rulePort 取规则的第一个端口，供排序使用；无端口或命名端口时返回 0。
//
// 直接读 IntVal 而非走 IntValue()：后者对命名端口会做字符串转数字，
// 排序不需要这一步，而 int -> int32 的强转会触发 gosec 溢出告警。
// IntOrString.IntVal 本身就是 int32，端口号构造时也从未越界过。
func rulePort(r Rule) int32 {
	var ports []networkingv1.NetworkPolicyPort
	switch {
	case r.Ingress != nil:
		ports = r.Ingress.Ports
	case r.Egress != nil:
		ports = r.Egress.Ports
	}
	if len(ports) == 0 || ports[0].Port == nil || ports[0].Port.Type != intstr.Int {
		return 0
	}
	return ports[0].Port.IntVal
}

// ruleProtocol 取规则的第一个协议，供排序使用；无端口时返回空串。
func ruleProtocol(r Rule) string {
	var ports []networkingv1.NetworkPolicyPort
	switch {
	case r.Ingress != nil:
		ports = r.Ingress.Ports
	case r.Egress != nil:
		ports = r.Egress.Ports
	}
	if len(ports) == 0 || ports[0].Protocol == nil {
		return ""
	}
	return string(*ports[0].Protocol)
}

// rulePeer 取规则的第一个对端，供排序使用。namespace 与 workload 来自
// selector 组合，cidr 来自 ipBlock；两种表达互斥，用不到的一律留空。
//
// PodSelector 的取值不按固定键取——peerSpec 按对端实际命中的标签键
// 构造 matchLabels，键可以是 app、k8s-app 等任意一种——而是直接取
// 这张单键 map 里唯一的值：peerSpec 生成的 podSelector 恒为单条
// matchLabels，遍历顺序在只有一个元素时不影响结果。
func rulePeer(r Rule) (ns, workload, cidr string) {
	var peers []networkingv1.NetworkPolicyPeer
	switch {
	case r.Ingress != nil:
		peers = r.Ingress.From
	case r.Egress != nil:
		peers = r.Egress.To
	}
	if len(peers) == 0 {
		return "", "", ""
	}
	p := peers[0]
	if p.IPBlock != nil {
		return "", "", p.IPBlock.CIDR
	}
	if p.NamespaceSelector != nil {
		ns = p.NamespaceSelector.MatchLabels[nsNameLabel]
	}
	if p.PodSelector != nil {
		for _, v := range p.PodSelector.MatchLabels {
			workload = v
		}
	}
	return ns, workload, ""
}

// dedupeGaps 按 (flowID, reason) 去重并定序。
//
// 一条流量的两侧可能报出同一个原因，界面上重复列出会把缺口的规模
// 显示成实际的两倍。
func dedupeGaps(in []UngeneratableItem) []UngeneratableItem {
	type gapKey struct {
		flowID string
		reason UngeneratableReason
	}
	seen := map[gapKey]bool{}
	out := make([]UngeneratableItem, 0, len(in))
	for _, it := range in {
		k := gapKey{it.FlowID, it.Reason}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].FlowID < out[j].FlowID
	})
	return out
}

// IngressSuffix 与 EgressSuffix 是拆分后两个对象名的后缀
// （design doc 2026-08-24 §3.6）。
//
// 导出成常量而不是各处写字面量：写回的文件名由对象名推出，前端也要按方向
// 分组，三处各写一遍就给了「其中一处改了后缀」一个位置，而那时文件与对象
// 对不上，症状是 GitOps 侧凭空多出一份策略、少删一份。
const (
	IngressSuffix = "-ingress"
	EgressSuffix  = "-egress"
)

// EnabledPolicies 把启用的规则渲染成可供回放的 NetworkPolicy 列表。
//
// 只吐启用规则：待确认的风险规则不进入生效策略集，否则 dry-run 预测
// 出的就不是"默认推荐上线后会怎样"，而是"把所有敞口都放开后会怎样"。
//
// **一个主体拆成两个对象：入站一个、出站一个**（design doc 2026-08-24 §3.6）。
// NetworkPolicy 是 additive 的，两个对象合起来与一个带两个方向的对象逐条等价；
// 拆开是为了让评审落到方向上 —— egress 收错的症状是隐蔽的超时，ingress 收错
// 是立刻连不上，两者该看的人和该问的问题都不一样。
//
// **没有规则的那一半照样生成，不能省。** 一个 policyTypes:[Ingress] 且 ingress
// 为空的对象，含义是「拒绝全部入站」，不是「无操作」。少生成它，那个主体的
// 入站就从默认拒绝变成全部放行 —— 方向朝不安全，且不报错。这也是拆分前
// 「固定 policyTypes 为 Ingress+Egress、规则为空即 default-deny」那条约定的
// 延续，不是对它的放宽。
func (r Result) EnabledPolicies() []networkingv1.NetworkPolicy {
	out := make([]networkingv1.NetworkPolicy, 0, 2*len(r.Policies))
	for _, p := range r.Policies {
		// 名字与 podSelector 按粒度分支。未登记的取值按 WORKLOAD 处理：
		// 那是现状、也是更精确的那一侧，失败方向朝窄（安全规范 §49）。
		// 落到 namespace 粒度会把一份本该只选中一个 workload 的策略
		// 变成选中整个命名空间 —— 那个方向不能靠零值走到。
		name := "candidate-" + p.Workload
		selector := metav1.LabelSelector{
			MatchLabels: map[string]string{p.WorkloadLabelKey: p.Workload},
		}
		if p.Granularity == GranularityNamespace {
			// 每个 namespace 内的常量名，因此天然唯一。空 selector 选中该
			// namespace 的全部 Pod —— 那正是这个粒度的定义。
			name, selector = "candidate-namespace", metav1.LabelSelector{}
		}
		in := networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: p.Namespace, Name: name + IngressSuffix,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			},
		}
		eg := networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: p.Namespace, Name: name + EgressSuffix,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: selector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		}
		for _, rule := range p.Rules {
			if !rule.Enabled {
				continue
			}
			if rule.Ingress != nil {
				in.Spec.Ingress = append(in.Spec.Ingress, *rule.Ingress)
			}
			if rule.Egress != nil {
				eg.Spec.Egress = append(eg.Spec.Egress, *rule.Egress)
			}
		}
		// 两个都进，包括规则为空的那个 —— 见函数注释。
		out = append(out, in, eg)
	}
	return out
}
