// Package reconcile 把平台回放算出的判定与执行平面自己报的判定对账
// （design doc 2026-08-25 §3）。
//
// 本包是纯函数：不做任何 I/O，不依赖集群或数据库。它回答的是「平台算的与
// 集群实际执行的差多少」——这是这个平台唯一一个可以在**生产流量上**度量的
// 可信度指标，而不是靠一组手写用例。
package reconcile

import (
	"sort"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/replay"
)

// Class 是一次对账的结论，封闭枚举。
type Class string

const (
	// ClassAgree 表示两边一致。
	ClassAgree Class = "AGREE"
	// ClassSourceSilent 表示来源根本没报判定（conntrack 源恒为此类）。
	//
	// **不参与一致率计算。** 把"没报"算进分母会让一个什么都不报的来源
	// 拿到 0% 一致率，而它其实什么都没说；算进分子则更糟。
	ClassSourceSilent Class = "SOURCE_SILENT"
	// ClassPlatformUnknown 表示平台答不出、而执行平面有答案。
	//
	// **不算分歧，算未覆盖。** 平台明说"我不知道"不是错，它只是没帮上忙；
	// 与"我算错了"混在一起会让真正的分歧被稀释。
	ClassPlatformUnknown Class = "PLATFORM_UNKNOWN"
	// ClassOverPermissive 表示平台判 ALLOW、执行平面实际拦下了。
	//
	// 平台**高估**了放行面。后果是把一条本来就不通的连接学成"需要放行"，
	// 于是多生成一条规则：安全性变差，可用性无损。
	ClassOverPermissive Class = "DISAGREE_OVER_PERMISSIVE"
	// ClassUnderPermissive 表示平台判 DENY、执行平面实际放行了。
	//
	// **这是唯一一条能绕过 dry-run 造成生产阻断的路径。**
	//
	// 平台以为这条连接现在就不通，于是不会为它生成放行规则；而集群里它通着
	// （被平台看不见的 CNP / ANP / mesh 放行，或引擎漏了一条允许规则）。
	// 候选集下发后它从通变成不通 —— 而 dry-run 会把它算成 UNCHANGED，
	// 因为在平台的世界里它本来就不通。
	ClassUnderPermissive Class = "DISAGREE_UNDER_PERMISSIVE"
)

// AllClasses 是枚举的唯一登记处，供统计口径与界面共用。
func AllClasses() []Class {
	return []Class{
		ClassAgree, ClassSourceSilent, ClassPlatformUnknown,
		ClassOverPermissive, ClassUnderPermissive,
	}
}

// Classify 判定一条连接上两边的关系。
//
// reported 为 false 表示来源没报判定 —— 与"报了放行"是两件事，因此它先于
// 其余分支判断（flow.Connection.Verdict 的两值返回正是为了让这个位置不可
// 被忽略）。
func Classify(platform replay.Verdict, observed flow.Verdict, reported bool) Class {
	if !reported {
		return ClassSourceSilent
	}
	switch platform {
	case replay.VerdictUnknown:
		return ClassPlatformUnknown
	case replay.VerdictAllow:
		if observed == flow.VerdictDenied {
			return ClassOverPermissive
		}
		return ClassAgree
	case replay.VerdictDeny:
		if observed == flow.VerdictAllowed {
			return ClassUnderPermissive
		}
		return ClassAgree
	default:
		// 平台给出一个本包不认识的判定时，不能算成一致 —— 那是把一个
		// 说不清的东西计入分子。归到"平台没答上"这一档。
		return ClassPlatformUnknown
	}
}

// Subject 是聚合的主体：一致率必须按 workload 看，整集群的平均值会把一个
// 全错的 workload 藏进几千条正确判定里。
type Subject struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
}

// Label 是主体在文案与界面上的写法。
//
// **workload 为空时必须说清为什么**：那不是"没有主体"，而是这些 Pod 上一个
// 归属标签（app / app.kubernetes.io/name / k8s-app / component）都没有 ——
// 平台因此既生成不出按 workload 的候选策略，也没法把分歧挂到谁头上。
// 渲染成 "ns/" 这种断尾，操作者拿着它无法行动。
func (s Subject) Label() string {
	if s.Workload == "" {
		return s.Namespace + "/（这些 Pod 没有 workload 归属标签）"
	}
	return s.Namespace + "/" + s.Workload
}

// Counts 是一个主体上各类结论的条数。
type Counts map[Class]int

// Report 是一次对账的完整结论。
type Report struct {
	// Total 是参与对账的连接总数，含未参与计算的那几类。
	Total int `json:"total"`
	// Overall 是整集群的分类计数。
	Overall Counts `json:"overall"`
	// BySubject 是按主体的分类计数，按 (namespace, workload) 排序。
	BySubject []SubjectCounts `json:"bySubject"`
	// Samples 是两类分歧的抽样证据，每 (主体, 类别) 至多 MaxSamplesPerClass 条。
	//
	// **只抽分歧。** AGREE 量最大且没有下钻价值；SOURCE_SILENT 与
	// PLATFORM_UNKNOWN 不是"我们算错了"，前者来源什么都没说，后者平台明说
	// 不知道。把它们一并存下来，样本表就会按流量规模增长。
	Samples []Sample `json:"samples"`
}

// SubjectCounts 是一个主体的对账结果。
type SubjectCounts struct {
	Subject Subject `json:"subject"`
	Counts  Counts  `json:"counts"`
}

// AgreementRate 是一致率：AGREE / (AGREE + 两类分歧)。
//
// **分母不含 SOURCE_SILENT 与 PLATFORM_UNKNOWN**（§3.2）：前者是来源什么都
// 没说，后者是平台明说不知道。把它们算进分母，一个数据源换成 conntrack
// 就能让一致率跌到 0，而引擎的正确性一点没变。
//
// 第二个返回值为 false 表示分母为零 —— **没有可比对的连接，就没有一致率**。
// 返回 0 会被读成"全错"，返回 1 会被读成"全对"，两者都是编的。
func (c Counts) AgreementRate() (float64, bool) {
	comparable := c[ClassAgree] + c[ClassOverPermissive] + c[ClassUnderPermissive]
	if comparable == 0 {
		return 0, false
	}
	return float64(c[ClassAgree]) / float64(comparable), true
}

// UnderPermissiveRate 是「平台低估放行面」的占比，取的是同一个分母。
//
// 单独给它一个方法而不是让调用方自己算：这一类是唯一能造成生产阻断的分歧，
// 门禁按它判（§3.4），而一个每处各算一遍的比率迟早会有两个定义。
func (c Counts) UnderPermissiveRate() (float64, bool) {
	comparable := c[ClassAgree] + c[ClassOverPermissive] + c[ClassUnderPermissive]
	if comparable == 0 {
		return 0, false
	}
	return float64(c[ClassUnderPermissive]) / float64(comparable), true
}

// MaxSamplesPerClass 是每个 (主体, 类别) 保留的分歧样本条数。
//
// **不全存**（design doc 2026-08-25 §3.4）：分歧量随流量规模增长，全存会让
// 账单失控，而这个平台的失控方向是账单不是性能（CLAUDE.md §5）。
//
// 五条足以看出模式 —— 同一个端口反复出现、还是散在几十个端口上，是两种完全
// 不同的排查方向。再多不会让结论更清楚，只会让表更大。
const MaxSamplesPerClass = 5

// Sample 是一条留下来供下钻的分歧证据。
//
// 门禁按分歧率拦人（§3.4），而"payment/api 20%"本身给不出下一步 —— 操作者
// 要看的是**哪几条连接**对不上，才能判断平台漏了什么（多半是它不解释的另一
// 个策略平面）。没有样本，那道门只能拦住人，不能告诉他怎么办。
type Sample struct {
	Subject Subject `json:"subject"`
	Class   Class   `json:"class"`
	// Flow 是那条连接本身：源端、目的端、协议与端口。
	Flow replay.Flow `json:"-"`
}

// Observation 是对账的一条输入。
type Observation struct {
	// Subject 是这条连接归属的主体，取源端 —— 候选规则按源端 workload 生成，
	// 门禁也按它拦，因此对账必须按同一个主体聚合。
	Subject Subject
	// Platform 是平台回放算出的判定。
	Platform replay.Verdict
	// Observed 与 Reported 是执行平面报的判定，以及它到底报没报。
	Observed flow.Verdict
	Reported bool
	// Flow 是这条连接本身，只在被抽成样本时用到。
	//
	// 带在输入上而不是让落库层回头再查一次：对账比较的是**这一次求值**得出
	// 的判定，而一次二次查询拿到的可能已经是另一批连接（窗口边界一漂就会）。
	Flow replay.Flow
}

// sampled 报告这一类是否值得留证据。
func sampled(c Class) bool {
	return c == ClassUnderPermissive || c == ClassOverPermissive
}

// classRank 决定样本的展示次序：能造成阻断的那一类排在最前面。
func classRank(c Class) int {
	if c == ClassUnderPermissive {
		return 0
	}
	return 1
}

// Run 对一批观测做一次对账。
func Run(in []Observation) Report {
	rep := Report{Total: len(in), Overall: Counts{}}
	for _, k := range AllClasses() {
		rep.Overall[k] = 0
	}
	bySubject := map[Subject]Counts{}
	// 抽样配额按 (主体, 类别) 各算各的：一个分歧极多的 workload 不该把别人的
	// 名额吃光，而"每个主体都有几条证据"正是按主体排查的前提。
	type quota struct {
		Subject
		Class
	}
	taken := map[quota]int{}
	var samples []Sample

	for _, o := range in {
		class := Classify(o.Platform, o.Observed, o.Reported)
		rep.Overall[class]++
		if sampled(class) {
			k := quota{o.Subject, class}
			// 取**前 N 条**，不随机：同一批输入两次跑出的样本必须相同，
			// 否则一份报告刷新一次就换一批证据，操作者无从核对。偏向窗口
			// 早期是已知且可预测的偏差，比不可复现的取样有用。
			if taken[k] < MaxSamplesPerClass {
				taken[k]++
				samples = append(samples, Sample{Subject: o.Subject, Class: class, Flow: o.Flow})
			}
		}
		c, ok := bySubject[o.Subject]
		if !ok {
			c = Counts{}
			for _, k := range AllClasses() {
				c[k] = 0
			}
			bySubject[o.Subject] = c
		}
		c[class]++
	}

	rep.Samples = samples
	rep.BySubject = make([]SubjectCounts, 0, len(bySubject))
	for s, c := range bySubject {
		rep.BySubject = append(rep.BySubject, SubjectCounts{Subject: s, Counts: c})
	}
	// 排序输出：这份报告会进指纹之外的很多地方（界面、指标、门禁），
	// 而一个随 map 遍历顺序变化的清单会让同一批数据每次读起来都不一样。
	// 样本排序：UNDER_PERMISSIVE 在前 —— 它是唯一能造成生产阻断的那一类，
	// 而翻页翻不到的证据等于没有。同类内保持输入次序（稳定排序）。
	sort.SliceStable(rep.Samples, func(i, j int) bool {
		a, b := rep.Samples[i], rep.Samples[j]
		if a.Class != b.Class {
			return classRank(a.Class) < classRank(b.Class)
		}
		if a.Subject.Namespace != b.Subject.Namespace {
			return a.Subject.Namespace < b.Subject.Namespace
		}
		return a.Subject.Workload < b.Subject.Workload
	})
	sort.Slice(rep.BySubject, func(i, j int) bool {
		if rep.BySubject[i].Subject.Namespace != rep.BySubject[j].Subject.Namespace {
			return rep.BySubject[i].Subject.Namespace < rep.BySubject[j].Subject.Namespace
		}
		return rep.BySubject[i].Subject.Workload < rep.BySubject[j].Subject.Workload
	})
	return rep
}
