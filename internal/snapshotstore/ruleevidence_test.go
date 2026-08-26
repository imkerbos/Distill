package snapshotstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// 证据跨窗口累加：窗口数递增、首末观测各自往两端扩、观测次数累计。
//
// 这是这张表存在的全部意义 —— 候选集是每次读时现算的，算出来的条数只描述
// 当前那一个窗口；「这条规则我们观察了多久」现算不出来。
func TestRuleEvidenceAccumulatesAcrossWindows(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	fp := "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

	rec := func(from time.Time, obs int64, complete bool) {
		t.Helper()
		if err := s.RecordRuleEvidence(ctx, clusterA, from, from.Add(time.Minute), complete,
			[]snapshotstore.RuleEvidence{{
				Fingerprint: fp, Namespace: "payment", Workload: "api", Observations: obs,
			}}); err != nil {
			t.Fatalf("RecordRuleEvidence() = %v", err)
		}
	}

	// 刻意先记晚的窗口，再记早的：补采是真实存在的形态，而首末观测必须
	// 各自往两端扩，不是"后写的覆盖先写的"。
	rec(base.Add(2*time.Hour), 30, true)
	rec(base, 12, false)

	got, err := s.RuleEvidenceOf(ctx, clusterA)
	if err != nil {
		t.Fatalf("RuleEvidenceOf() = %v", err)
	}
	e, ok := got[snapshotstore.EvidenceKey("payment", "api", fp)]
	if !ok {
		t.Fatalf("证据表里没有这条规则，拿到的是 %v", got)
	}
	if e.Windows != 2 {
		t.Errorf("Windows = %d, want 2", e.Windows)
	}
	if e.CompleteWindows != 1 {
		t.Errorf("CompleteWindows = %d, want 1 —— 两个窗口里只有一个证明得了自己没漏",
			e.CompleteWindows)
	}
	if e.Observations != 42 {
		t.Errorf("Observations = %d, want 42（12 + 30 累计）", e.Observations)
	}
	if !e.FirstSeen.Equal(base) {
		t.Errorf("FirstSeen = %v, want %v —— 补采把首次观测往后推了", e.FirstSeen, base)
	}
	if !e.LastSeen.Equal(base.Add(2*time.Hour + time.Minute)) {
		t.Errorf("LastSeen = %v, want %v", e.LastSeen, base.Add(2*time.Hour+time.Minute))
	}
	if e.Namespace != "payment" || e.Workload != "api" {
		t.Errorf("主体 = %s/%s, want payment/api", e.Namespace, e.Workload)
	}
}

// 一个规则都没有时不报错、也不写任何行。
//
// 一次"这个窗口什么规则都没生成"的采集是正常形态（新集群、零流量），
// 它不该变成一次失败。
func TestRecordingNoRulesIsNotAnError(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.RecordRuleEvidence(context.Background(), clusterA,
		time.Now().UTC(), time.Now().UTC(), true, nil); err != nil {
		t.Errorf("RecordRuleEvidence(nil) = %v, want nil", err)
	}
	got, err := s.RuleEvidenceOf(context.Background(), clusterA)
	if err != nil || len(got) != 0 {
		t.Errorf("RuleEvidenceOf() = %v, %v, want empty", got, err)
	}
}

// 两个 workload 共用同一条规则时，证据各记各的。
//
// 规则指纹只覆盖规则内容（policygen.FingerprintOf），不含主体 —— "egress
// 到 kube-dns:53" 在集群里每个 workload 上都是同一个指纹。证据若只按指纹
// 归集，一个窗口里 40 个 workload 就会把 windows 加 40 次，一次采集看起来
// 像观察了 40 个窗口。这与 rule_override 的主键形状必须一致。
func TestEvidenceIsScopedToTheSubjectNotJustTheFingerprint(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	from := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	fp := "dd" + "00000000000000000000000000000000000000000000000000000000000000"

	if err := s.RecordRuleEvidence(ctx, clusterA, from, from.Add(time.Minute), true,
		[]snapshotstore.RuleEvidence{
			{Fingerprint: fp, Namespace: "payment", Workload: "api", Observations: 5},
			{Fingerprint: fp, Namespace: "payment", Workload: "worker", Observations: 7},
		}); err != nil {
		t.Fatalf("RecordRuleEvidence() = %v", err)
	}

	got, err := s.RuleEvidenceOf(ctx, clusterA)
	if err != nil {
		t.Fatalf("RuleEvidenceOf() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("拿到 %d 条证据，want 2 —— 两个主体的证据撞成了一条: %v", len(got), got)
	}
	for _, key := range []string{
		snapshotstore.EvidenceKey("payment", "api", fp),
		snapshotstore.EvidenceKey("payment", "worker", fp),
	} {
		e, ok := got[key]
		if !ok {
			t.Fatalf("证据里缺 %s，拿到的是 %v", key, got)
		}
		if e.Windows != 1 {
			t.Errorf("%s 的 Windows = %d, want 1 —— 同窗口内的另一个主体把它加上去了",
				key, e.Windows)
		}
	}
}
