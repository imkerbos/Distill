package collectstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
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
