package policygen

import (
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
)

// ImportedPolicy 是一条人工导入、要加进候选集的策略
// （design doc 2026-08-25-existing-policies §3；registry.RoleCandidateAddition）。
//
// **它存在的理由是观测看不见的东西。** 月结批处理、季度对账、只在故障时走的
// 灾备链路 —— 这些连接不在观测窗口里就学不出规则，而 dry-run 也报不出来
// （它只评估见过的连接）。学习期门槛（design doc 2026-08-25 §5）把这条风险与
// 一个有人签字的判断绑在一起，而这里是操作者**补上那条规则**的入口。
type ImportedPolicy struct {
	// ImportID 是登记标识，用来把生成出来的规则指回那条导入记录。
	ImportID string
	// Policy 是解析好的策略对象（registry.ParseImport 的产物）。
	Policy networkingv1.NetworkPolicy
}

// UnattachedImport 是一条挂不到任何主体上的导入。
//
// **报出来而不是静默丢掉**，与 ExcludedWorkloads 同一条纪律：一条导入进来了
// 却没有出现在候选集里，操作者会以为它生效了 —— 而它恰恰是用来补那条平台
// 看不见的连接的，"以为补上了"比"知道没补上"危险得多。
type UnattachedImport struct {
	ImportID  string `json:"importId"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Reason 是封闭枚举，不是自由文本。
	Reason UnattachedReason `json:"reason"`
}

// UnattachedReason 是导入挂不上主体的原因。封闭枚举。
type UnattachedReason string

const (
	// UnattachedNoWorkloadLabel 表示 podSelector 里没有 workload 归属标签。
	//
	// 候选集按 (namespace, workload) 组织，一条选不出主体的策略挂不上去。
	// 典型形态是空 podSelector（选中整个 namespace）或只用 matchExpressions。
	// **不硬塞进某个主体**：塞错的后果是这条规则跟着一个不相干的 workload
	// 一起被写回或被禁用。
	UnattachedNoWorkloadLabel UnattachedReason = "NO_WORKLOAD_LABEL"
	// UnattachedNoRules 表示这条策略一条 ingress/egress 规则都没有。
	//
	// 空规则在 NetworkPolicy 语义里是 default-deny，那是**收紧**，而导入这条
	// 路只接受补充放行（RoleCandidateAddition）。要收紧走的是策略生成本身。
	UnattachedNoRules UnattachedReason = "NO_RULES"
	// UnattachedNoSuchWorkload 表示这个集群里没有该主体的 Pod。
	//
	// 挂上去会生成一条选不中任何 Pod 的幽灵策略：它不报错，只是永远不生效，
	// 而操作者以为自己补上了那条放行 —— 与 ExclusionLabelKeyConflict 同一
	// 处置，点名报出来好过发一条谁都选不中的策略。
	//
	// 集群里暂时没有这个 workload 是正常形态（还没部署、缩到零），因此这
	// **不是**一个错误，是一条要被看见的状态。
	UnattachedNoSuchWorkload UnattachedReason = "NO_SUCH_WORKLOAD"
)

// importedRules 把导入的策略拆成挂在主体上的规则。
//
// 拆成 Rule 而不是把整个对象旁路塞进 EnabledPolicies：只有拆开，这些规则才
// 能出现在候选列表里、能被人工禁用、能进证据体系与 dry-run —— 一条旁路进去的
// 策略在界面上不可见也不可控，而它照样会被写进仓库。
//
// 返回的第二个值是挂不上主体的那些，必须由调用方报出去。
func importedRules(
	imports []ImportedPolicy, roster map[subject]bool,
) (map[subject][]Rule, []UnattachedImport) {
	out := map[subject][]Rule{}
	// 恒为切片而不是 nil：Generate 一定跑过这一段，因此"一条都没有"是一个
	// **算过的空集**。序列化成 null 会被读成"这一栏没人算过"，而那正是这栏
	// 要消除的状态（同 Rule.Peers 那条：[] 与 null 在界面上是两个意思）。
	unattached := []UnattachedImport{}

	// 名册按 (namespace, workload) 索引：导入的 podSelector 命中哪个标签键
	// 由它自己写死，而名册里那个 workload 的赢家键可能是另一个。以名册的
	// 键为准 —— 生成出来的策略必须选得中真实存在的 Pod。
	byNSWorkload := map[nsWorkload]subject{}
	for s := range roster {
		byNSWorkload[nsWorkload{namespace: s.namespace, workload: s.workload}] = s
	}

	for _, imp := range imports {
		p := imp.Policy
		_, workload, ok := WorkloadOf(p.Spec.PodSelector.MatchLabels)
		if !ok {
			unattached = append(unattached, UnattachedImport{
				ImportID: imp.ImportID, Namespace: p.Namespace, Name: p.Name,
				Reason: UnattachedNoWorkloadLabel,
			})
			continue
		}
		rules := rulesOf(p)
		if len(rules) == 0 {
			unattached = append(unattached, UnattachedImport{
				ImportID: imp.ImportID, Namespace: p.Namespace, Name: p.Name,
				Reason: UnattachedNoRules,
			})
			continue
		}
		key, inRoster := byNSWorkload[nsWorkload{namespace: p.Namespace, workload: workload}]
		if !inRoster {
			unattached = append(unattached, UnattachedImport{
				ImportID: imp.ImportID, Namespace: p.Namespace, Name: p.Name,
				Reason: UnattachedNoSuchWorkload,
			})
			continue
		}
		out[key] = append(out[key], rules...)
	}
	return out, unattached
}

// rulesOf 把一条策略的每一段 ingress/egress 各变成一条规则。
//
// 逐段拆而不是整条策略一条规则：候选列表是按规则读的，人工确认也挂在规则
// 指纹上（rule_override）。一整条策略折成一行，操作者就只能整条禁用，而他
// 想否掉的多半只是其中一段。
func rulesOf(p networkingv1.NetworkPolicy) []Rule {
	var out []Rule
	for i := range p.Spec.Ingress {
		out = append(out, importedRule(replay.DirectionIngress, &p.Spec.Ingress[i], nil))
	}
	for i := range p.Spec.Egress {
		out = append(out, importedRule(replay.DirectionEgress, nil, &p.Spec.Egress[i]))
	}
	return out
}

// importedRule 造一条导入来源的规则。
//
// **默认启用。** 与学习来的规则不同：那些是平台从流量里猜出来的，因此要人
// 确认；这一条是操作者写下来、带审计与 importedBy 的一次明示决定，再要一次
// 确认是把同一个决定问两遍。要否掉它，走的是与其余规则相同的人工禁用
// （rule_override 挂在指纹上）。
//
// 风险端口照旧标注但不因此停用：命中风险清单说明"这条放行值得看一眼"，
// 而操作者已经看过了 —— 他就是写下它的那个人。标注留着，是为了让**下一个**
// 读这一屏的人也看见。
func importedRule(
	dir replay.Direction,
	in *networkingv1.NetworkPolicyIngressRule,
	eg *networkingv1.NetworkPolicyEgressRule,
) Rule {
	r := Rule{
		Origin:    OriginImported,
		Direction: dir,
		Enabled:   true,
		// FlowCount 恒为 0，而且**这不是"没有流量"**：导入这条路存在的理由
		// 就是那条连接不在观测里。界面必须按来源解释这个 0，不能把它读成
		// "这条规则没人用、可以收紧"。
		FlowCount: 0,
		Ingress:   in,
		Egress:    eg,
	}
	if rp, risky := riskiestPort(portsOf(in, eg)); risky {
		r.Risk = &rp
	}
	r.describe()
	return r
}

// portsOf 取出规则体里的端口。
func portsOf(
	in *networkingv1.NetworkPolicyIngressRule, eg *networkingv1.NetworkPolicyEgressRule,
) []networkingv1.NetworkPolicyPort {
	switch {
	case in != nil:
		return in.Ports
	case eg != nil:
		return eg.Ports
	}
	return nil
}

// riskiestPort 返回这段规则里命中风险清单的第一个端口。
//
// 只标一个：Rule.Risk 是单值，而这一栏要回答的是"这条规则值不值得看一眼"，
// 不是"列全所有风险端口"。端口全文在 Ports 那一栏里。
//
// **端口写成名字（命名端口）或落在 1..65535 之外时不参与判定**：前者要对着
// 目标 Pod 的 containerPort 才解析得出来，这里没有那份名册；后者根本不是端口。
// 判不出来就不标，不猜 —— 尤其不能截断，见下面那段。
func riskiestPort(ports []networkingv1.NetworkPolicyPort) (risk.Port, bool) {
	for _, p := range ports {
		if p.Port == nil {
			continue
		}
		n := p.Port.IntValue()
		// **范围外的取值直接跳过，不截断。** IntValue 返回 int，硬转成 int32
		// 会让一个越界的数字折回成另一个**真实存在**的端口号 —— 于是一条写着
		// 70000 的规则可能命中 4464 的风险登记，或者反过来漏掉真正的风险端口。
		// 命名端口在这里也是 0（IntValue 对非数字取值返回 0），同样跳过：
		// 风险清单按数字端口登记，而把名字解析成数字要对着目标 Pod 的
		// containerPort，这里没有那份名册。判不出来就不标，不猜。
		if n < 1 || n > 65535 {
			continue
		}
		if rp, ok := risk.Lookup(int32(n)); ok {
			return rp, true
		}
	}
	return risk.Port{}, false
}
