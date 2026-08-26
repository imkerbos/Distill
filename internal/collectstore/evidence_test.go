package collectstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// 记过的证据要挂到对应的候选规则上，并且键与 rule_override 的主体形状一致。
//
// 候选集是现算的，flowCount 只描述当前那一个窗口；"这条规则我们看了多久"
// 只能从证据表来。两者对不上号，界面就只能显示当前窗口那一个数。
func TestPolicyPreviewCarriesRecordedEvidence(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	seedPreviewCluster(t, s)
	saveIngest(t, s, []flow.Connection{conn(recycledIP, peerIP, portResolved)})

	pv, err := r.PolicyPreview(ctx, collectedID, "", describedWindow())
	if err != nil {
		t.Fatalf("PolicyPreview() = %v", err)
	}
	if pv.Evidence == nil {
		t.Fatal("Evidence 为 nil —— 采集集群应当是「记过、暂时还没有」的空 map，" +
			"而 nil 的含义是「根本没在记」")
	}

	// 取屏幕上真实存在的一条规则，按它的主体与指纹记一次证据。
	var ns, wl, fp string
	for _, c := range pv.Candidates {
		for _, rule := range c.Rules {
			ns, wl, fp = c.Namespace, c.Workload, rule.Fingerprint
			break
		}
		if fp != "" {
			break
		}
	}
	if fp == "" {
		t.Fatal("候选集里一条规则都没有，这个用例无从验证")
	}

	from := windowStart.Add(-24 * time.Hour)
	if err := s.RecordRuleEvidence(ctx, collectedID, from, windowEnd, false,
		[]snapshotstore.RuleEvidence{{
			Fingerprint: fp, Namespace: ns, Workload: wl, Observations: 9,
		}}); err != nil {
		t.Fatalf("RecordRuleEvidence() = %v", err)
	}

	pv, err = r.PolicyPreview(ctx, collectedID, "", describedWindow())
	if err != nil {
		t.Fatalf("PolicyPreview() = %v", err)
	}
	e, ok := pv.Evidence[snapshotstore.EvidenceKey(ns, wl, fp)]
	if !ok {
		t.Fatalf("证据没挂到 %s/%s 的规则上，拿到的键是 %v", ns, wl, keysOf(pv.Evidence))
	}
	if e.Windows != 1 || e.Observations != 9 {
		t.Errorf("证据 = %d 窗口 / %d 次观测, want 1 / 9", e.Windows, e.Observations)
	}
	// 记的时候这个窗口证明不了自己没漏，读回来必须照样是 0：把它读成
	// "1 个完整窗口"会让一条其实没看全的证据显得已经站得住。
	if e.CompleteWindows != 0 {
		t.Errorf("CompleteWindows = %d, want 0", e.CompleteWindows)
	}
	if !e.FirstSeen.Equal(from) {
		t.Errorf("FirstSeen = %v, want %v —— 首次观测取的不是窗口边界", e.FirstSeen, from)
	}
}

func keysOf(m map[string]store.RuleEvidence) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 分歧被抽成可下钻的样本，且带上渲染好的两端与端口。
//
// 门禁按分歧率拦人（一致率门禁），而一个只有比率的界面给不出下一步：
// 操作者要看的是**哪几条连接**对不上，才能判断平台漏了什么。
//
// **必须先断言真的有样本。** 一个「遍历样本逐条检查」的用例在样本为空时
// 恒绿，而这里的默认形态恰恰是空的（fixture 的连接不报判定，全是
// SOURCE_SILENT）—— 写这条用例的第一版就是这么假绿的。
func TestReconciliationCarriesDrillableSamples(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	seedPreviewCluster(t, s)

	// 造一条真 UNDER_PERMISSIVE：种子里的 allow-api 没写 policyTypes，
	// 按 k8s 语义即 policyTypes: [Ingress] 且规则为空 —— 对 payment/api 的
	// default-deny ingress。因此发往它的连接平台判 DENY，而这里让执行平面
	// 报"放行"，正是那条最危险的分歧的现实形态：集群里有平台看不见的东西
	// 放行了它（另一个策略平面），平台却以为这条路本来就不通。
	c := conn(peerIP, recycledIP, portResolved).WithVerdict(flow.VerdictAllowed)
	saveIngest(t, s, []flow.Connection{c})

	rec, err := r.Reconciliation(ctx, collectedID, describedWindow())
	if err != nil {
		t.Fatalf("Reconciliation() = %v", err)
	}
	if !rec.SourceReportsVerdicts {
		t.Fatal("来源没报判定，这个窗口根本对不了账 —— 用例的前提没建立起来")
	}
	if len(rec.Samples) == 0 {
		t.Fatalf("一条样本都没有，下面的逐条检查等于没跑。分类计数：%v", rec.Report.Overall)
	}

	for _, smp := range rec.Samples {
		if smp.Class != "DISAGREE_UNDER_PERMISSIVE" && smp.Class != "DISAGREE_OVER_PERMISSIVE" {
			t.Errorf("抽到了 %s 类样本，它没有下钻价值", smp.Class)
		}
		if smp.Source == "" || smp.Dest == "" {
			t.Errorf("样本的端点是空的，读的人无从判断这条连接是谁到谁：%+v", smp)
		}
		if smp.Port == 0 {
			t.Errorf("样本没有端口，一条不知道端口的分歧无从排查：%+v", smp)
		}
		if smp.At.IsZero() {
			t.Errorf("样本没有连接发生的时刻，无法对齐历史快照：%+v", smp)
		}
	}
}

// 人工导入的补充规则真的进候选集，而 BASELINE_CURRENT 不进。
//
// 导入这条路存在的理由是**观测看不见的东西**：月结批处理、季度对账、只在
// 故障时走的灾备链路 —— 不在窗口里就学不出规则，而 dry-run 也报不出来。
// 它是学习期门槛那条根本限制的人工补救入口。
//
// 两个角色必须分开：BASELINE_CURRENT 描述的是"集群当前跑着什么"，属于回放的
// current 侧；混进候选集会让一条用于描述现状的策略变成一条平台推荐下发的规则。
func TestCandidateAdditionImportsReachTheCandidateSet(t *testing.T) {
	const addition = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: monthly-settlement
  namespace: payment
spec:
  podSelector:
    matchLabels:
      app: api
  policyTypes:
  - Egress
  egress:
  - to:
    - ipBlock:
        cidr: 10.9.0.0/28
    ports:
    - port: 5432
      protocol: TCP
`
	// 同一份内容登记成 BASELINE_CURRENT，它不该出现在候选集里。
	current := strings.Replace(addition, "monthly-settlement", "describes-current", 1)

	r, s := newTestReaderWithImports(t, map[string][]registry.PolicyImport{
		collectedID: {
			{
				ClusterID: collectedID, ImportID: "imp-addition",
				Role: registry.RoleCandidateAddition, Source: registry.SourcePaste,
				Namespace: "payment", Name: "monthly-settlement", YAML: addition,
			},
			{
				ClusterID: collectedID, ImportID: "imp-current",
				Role: registry.RoleBaselineCurrent, Source: registry.SourcePaste,
				Namespace: "payment", Name: "describes-current", YAML: current,
			},
		},
	})
	seedPreviewCluster(t, s)
	saveIngest(t, s, []flow.Connection{conn(recycledIP, peerIP, portResolved)})

	pv, err := r.PolicyPreview(context.Background(), collectedID, "", describedWindow())
	if err != nil {
		t.Fatalf("PolicyPreview() = %v", err)
	}
	if len(pv.UnattachedImports) != 0 {
		t.Fatalf("导入没挂上：%+v", pv.UnattachedImports)
	}

	var imported int
	for _, c := range pv.Candidates {
		for _, rule := range c.Rules {
			if rule.Origin != policygen.OriginImported {
				continue
			}
			imported++
			if c.Namespace != "payment" || c.Workload != "api" {
				t.Errorf("导入规则挂到了 %s/%s，want payment/api", c.Namespace, c.Workload)
			}
			// 那个 0 不是"没有流量"，界面必须按来源解释它。
			if rule.FlowCount != 0 {
				t.Errorf("FlowCount = %d, want 0", rule.FlowCount)
			}
		}
	}
	if imported != 1 {
		t.Fatalf("候选集里有 %d 条导入规则, want 1 —— BASELINE_CURRENT 那条"+
			"描述的是现状，不该变成一条平台推荐下发的规则", imported)
	}

	// 导入的规则必须进得了生效策略集：进不去等于没补上，而操作者以为补上了。
	var inEnabled bool
	for _, p := range pv.Overridden.Enabled {
		if p.Namespace == "payment" {
			for _, e := range p.Spec.Egress {
				for _, port := range e.Ports {
					if port.Port != nil && port.Port.IntValue() == 5432 {
						inEnabled = true
					}
				}
			}
		}
	}
	if !inEnabled {
		t.Error("导入的放行没有进生效策略集，写回时它不会被写出去")
	}
}
