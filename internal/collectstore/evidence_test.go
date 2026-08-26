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
