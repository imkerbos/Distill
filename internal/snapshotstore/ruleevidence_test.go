package snapshotstore_test

import (
	"context"
	"fmt"
	"strings"
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

	got, err := s.RuleEvidenceOf(ctx, clusterA, []snapshotstore.EvidenceRef{
		{Namespace: "payment", Workload: "api", Fingerprint: fp},
	})
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
	got, err := s.RuleEvidenceOf(context.Background(), clusterA, []snapshotstore.EvidenceRef{
		{Namespace: "payment", Workload: "api", Fingerprint: "whatever"},
	})
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

	got, err := s.RuleEvidenceOf(ctx, clusterA, []snapshotstore.EvidenceRef{
		{Namespace: "payment", Workload: "api", Fingerprint: fp},
		{Namespace: "payment", Workload: "worker", Fingerprint: fp},
	})
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

// LastRuleEvidenceWindowEnd 报告这个集群的证据记到了哪个窗口末端。
//
// 推送式接入靠它避免把同一个窗口记两次：windows 是 `windows + 1`，按窗口
// 不幂等，而记账周期与 agent 推送周期是两条独立的节奏。
func TestLastRuleEvidenceWindowEndReportsTheFurthestWindow(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	fp := "b1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

	// 一条证据都没有时必须说"没记过"，而不是答一个零值时刻 —— 零值早于
	// 任何真实窗口，会让第一个窗口被当成"已经记过"而永远跳过。
	if _, ok, err := s.LastRuleEvidenceWindowEnd(ctx, clusterA); err != nil {
		t.Fatalf("LastRuleEvidenceWindowEnd() = %v", err)
	} else if ok {
		t.Error("一条证据都没有，却报告说记过了")
	}

	rec := func(from time.Time) {
		t.Helper()
		if err := s.RecordRuleEvidence(ctx, clusterA, from, from.Add(time.Minute), false,
			[]snapshotstore.RuleEvidence{{
				Fingerprint: fp, Namespace: "payment", Workload: "api", Observations: 1,
			}}); err != nil {
			t.Fatalf("RecordRuleEvidence() = %v", err)
		}
	}
	// 先晚后早：取的必须是最远的那一端，不是最后写进去的那一条。
	rec(base.Add(2 * time.Hour))
	rec(base)

	got, ok, err := s.LastRuleEvidenceWindowEnd(ctx, clusterA)
	if err != nil {
		t.Fatalf("LastRuleEvidenceWindowEnd() = %v", err)
	}
	if !ok {
		t.Fatal("记过两个窗口，却报告说没记过")
	}
	want := base.Add(2*time.Hour + time.Minute)
	if !got.Equal(want) {
		t.Errorf("LastRuleEvidenceWindowEnd() = %v, want %v", got, want)
	}

	// 集群之间不得串：另一个集群记过不等于这个集群记过。
	if _, ok, err := s.LastRuleEvidenceWindowEnd(ctx, clusterB); err != nil {
		t.Fatalf("LastRuleEvidenceWindowEnd(clusterB) = %v", err)
	} else if ok {
		t.Error("另一个集群的证据被当成了这个集群的")
	}
}

// **证据表长得再大，也不得让策略预览失败。**
//
// 这张表只供展示，不解锁任何门禁（design doc 2026-08-25 §4）。此前读取面
// 在行数超过上限时整个报错，而调用它的是 PolicyPreview —— 平台的主输出。
// 更糟的是记账本身要先算一次预览：表一旦过线，预览失败 → 记不了账 →
// 没有任何东西会让表变小，于是那个集群的策略页永久打不开，只能手工进库救。
//
// 读取面因此按**这一批候选自己的键**取，返回行数被候选数钉住，与表有多大无关。
func TestRuleEvidenceOfStaysUsableWhenTheTableIsHuge(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	// 造一张远超上限的表。行数取 20001 是刻意的：上限就是 20000。
	// 分批插入：一条语句 6 个占位符一行，两万行会撞 MySQL 的占位符上限。
	const (
		rows      = 20001
		seedBatch = 2000
	)
	for start := 0; start < rows; start += seedBatch {
		end := min(start+seedBatch, rows)
		var (
			sb   strings.Builder
			args []any
		)
		sb.WriteString(`INSERT INTO rule_evidence
		  (cluster_id, fingerprint, namespace, workload,
		   first_seen, last_seen, windows, complete_windows, observations) VALUES `)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteString(",")
			}
			sb.WriteString("(?,?,?,?,?,?,1,0,1)")
			args = append(args, clusterA, fmt.Sprintf("%064x", i), "payment",
				fmt.Sprintf("w%d", i), base, base.Add(time.Minute))
		}
		if _, err := db.ExecContext(ctx, sb.String(), args...); err != nil {
			t.Fatalf("seed rule_evidence: %v", err)
		}
	}

	// 这一批候选只认其中三条。
	want := []snapshotstore.EvidenceRef{
		{Namespace: "payment", Workload: "w7", Fingerprint: fmt.Sprintf("%064x", 7)},
		{Namespace: "payment", Workload: "w9", Fingerprint: fmt.Sprintf("%064x", 9)},
		{Namespace: "payment", Workload: "w11", Fingerprint: fmt.Sprintf("%064x", 11)},
	}
	got, err := s.RuleEvidenceOf(ctx, clusterA, want)
	if err != nil {
		t.Fatalf("RuleEvidenceOf() = %v —— 表大到过线就答不出，策略页会永久打不开", err)
	}
	if len(got) != len(want) {
		t.Fatalf("读回 %d 条，want %d —— 返回行数必须被候选数钉住", len(got), len(want))
	}
	for _, ref := range want {
		if _, ok := got[snapshotstore.EvidenceKey(ref.Namespace, ref.Workload, ref.Fingerprint)]; !ok {
			t.Errorf("候选里的 %s/%s 没有读回证据", ref.Namespace, ref.Workload)
		}
	}
}

// 候选里没有的键不得被读回来：多出来的证据没有落脚点，只会白占内存。
func TestRuleEvidenceOfReturnsOnlyTheRequestedRules(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	fpA := fmt.Sprintf("%064x", 0xaa)
	fpB := fmt.Sprintf("%064x", 0xbb)

	if err := s.RecordRuleEvidence(ctx, clusterA, base, base.Add(time.Minute), false,
		[]snapshotstore.RuleEvidence{
			{Fingerprint: fpA, Namespace: "payment", Workload: "api", Observations: 3},
			{Fingerprint: fpB, Namespace: "payment", Workload: "api", Observations: 5},
		}); err != nil {
		t.Fatalf("RecordRuleEvidence() = %v", err)
	}

	got, err := s.RuleEvidenceOf(ctx, clusterA, []snapshotstore.EvidenceRef{
		{Namespace: "payment", Workload: "api", Fingerprint: fpA},
	})
	if err != nil {
		t.Fatalf("RuleEvidenceOf() = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("读回 %d 条，want 1", len(got))
	}
	if _, ok := got[snapshotstore.EvidenceKey("payment", "api", fpB)]; ok {
		t.Error("没被问到的规则也读回来了")
	}
}

// 一条候选都没有时不查库，答空。
func TestRuleEvidenceOfWithoutAnyRuleAsksNothing(t *testing.T) {
	s, _ := newTestStore(t)
	got, err := s.RuleEvidenceOf(context.Background(), clusterA, nil)
	if err != nil {
		t.Fatalf("RuleEvidenceOf() = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("没有候选却读回 %d 条证据", len(got))
	}
}
