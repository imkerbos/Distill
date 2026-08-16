package snapshotstore_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// 这一条是 spec §4.2 的核心：一个因权限不足没采到的资源，摘要上
// 绝不能表现为一个条数。
//
// 计数表里「NetworkPolicy = 0」与「NetworkPolicy 因 FORBIDDEN 根本没采到」
// 写下来一模一样。只读计数的可见面会把后者显示成「这个集群没有任何策略」——
// 那是"结果比现实好看"的方向，而这个平台的判定必须往反方向错。
//
// 三个断言缺一不可：库里确实存着那个 0（所以这不是"数据没写"），
// Count() 拒绝把它交出来，且序列化后的报文里根本没有那个数字 ——
// 最后一条是真正受力的那个，前端拿不到数字就渲染不出数字。
func TestForbiddenResourceNeverReportsACount(t *testing.T) {
	s, db := newTestStore(t)

	run := sampleRun(clusterA, "run-forbidden", runOneAt)
	run.Status = snapshot.RunPartial
	run.Observation.Policies = nil // 没被授权看，所以一条也没采到
	run.Failures = []snapshot.Failure{{
		Resource: snapshot.ResourceNetworkPolicy,
		Reason:   snapshot.FailureForbidden,
		Detail:   `networkpolicies.networking.k8s.io is forbidden`,
	}}
	mustSave(t, s, run)

	// 库里确实有一行 item_count = 0：下面两条断言拦下的是读取侧的解释，
	// 不是一份缺了数据的表。
	if n := scanInt(t, db,
		`SELECT item_count FROM collection_run_resource
		  WHERE cluster_id = ? AND run_id = 'run-forbidden' AND resource = 'NETWORKPOLICY'`,
		clusterA); n != 0 {
		t.Fatalf("stored NETWORKPOLICY item_count = %d, want 0 (the row the read side must not hand out)", n)
	}

	got, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}

	policies := outcomeOf(t, got.Resources, "NETWORKPOLICY")
	if n, observed := policies.Count(); observed {
		t.Errorf("NETWORKPOLICY Count() = %d, observed = true; a FORBIDDEN resource has no count", n)
	}

	// 报文里没有 count 键 —— 数字不在线上，下游就渲染不出来。
	fields := marshalResource(t, policies)
	if raw, ok := fields["count"]; ok {
		t.Errorf("marshalled NETWORKPOLICY carries count = %s; a FORBIDDEN resource must not put a number on the wire", raw)
	}
	if _, ok := fields["failure"]; !ok {
		t.Errorf("marshalled NETWORKPOLICY = %v, want a failure object", fields)
	}
}

// 采到了的资源反过来必须带上数字，且不带失败。
//
// 与上一条成对：只证"失败时没有数字"的话，一个永远不输出数字的实现
// 也能通过，而那个实现同样是错的。
func TestObservedResourceReportsItsCountAndNoFailure(t *testing.T) {
	s, _ := newTestStore(t)
	mustSave(t, s, sampleRun(clusterA, "run-ok", runOneAt))

	got, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}

	pods := outcomeOf(t, got.Resources, "POD")
	if n, observed := pods.Count(); !observed || n != 1 {
		t.Errorf("POD Count() = %d (observed=%v), want 1, true", n, observed)
	}
	if record, failed := pods.Failure(); failed {
		t.Errorf("POD Failure() = %+v, want no failure", record)
	}

	fields := marshalResource(t, pods)
	if string(fields["count"]) != "1" {
		t.Errorf("marshalled POD count = %s, want 1", fields["count"])
	}
	if _, ok := fields["failure"]; ok {
		t.Errorf("marshalled POD carries a failure key: %v", fields)
	}
}

// ResourceReplicaSet 在枚举里但从不计数 —— 它只用于解 owner 链，
// 不是被观测的资产。它的缺席不得被读成一次采集失败（spec §4.2）。
//
// 按枚举补齐摘要是很自然的写法，而它会在每一次完全成功的采集上
// 凭空产生一条 REPLICASET 失败。一个永远带着失败的摘要，等于没有摘要：
// 操作者会学会忽略它，然后连真的 FORBIDDEN 一起忽略掉。
func TestReplicaSetAbsenceIsNotAFailure(t *testing.T) {
	s, _ := newTestStore(t)
	mustSave(t, s, sampleRun(clusterA, "run-ok", runOneAt))

	got, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.Status != string(snapshot.RunOK) {
		t.Errorf("Status = %q, want OK", got.Status)
	}

	for _, o := range got.Resources {
		if o.Resource == string(snapshot.ResourceReplicaSet) {
			t.Errorf("Resources contains %s; it is never counted and must not appear at all",
				snapshot.ResourceReplicaSet)
		}
		if record, failed := o.Failure(); failed {
			t.Errorf("%s reported a failure %+v on a fully successful run", o.Resource, record)
		}
	}
}

// 反过来：ReplicaSet 真的采失败时，那条失败必须出现 —— 即使计数表里
// 从来没有它的行。
//
// 以计数表为准去 join 失败表是另一种自然写法，它会让"这一类根本没能
// 列出来"的失败在摘要上彻底消失，而消失看起来就像一切正常。
func TestAFailureWithNoCountRowStillSurfaces(t *testing.T) {
	s, db := newTestStore(t)

	run := sampleRun(clusterA, "run-rs-forbidden", runOneAt)
	run.Status = snapshot.RunPartial
	run.Failures = []snapshot.Failure{{
		Resource: snapshot.ResourceReplicaSet,
		Reason:   snapshot.FailureForbidden,
		Detail:   `replicasets.apps is forbidden`,
	}}
	mustSave(t, s, run)

	// 前提确认：计数表里确实没有 REPLICASET 这一行。
	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM collection_run_resource
		  WHERE cluster_id = ? AND run_id = 'run-rs-forbidden' AND resource = 'REPLICASET'`,
		clusterA); n != 0 {
		t.Fatalf("collection_run_resource has %d REPLICASET rows, want 0", n)
	}

	got, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}

	replicaSets := outcomeOf(t, got.Resources, string(snapshot.ResourceReplicaSet))
	record, failed := replicaSets.Failure()
	if !failed {
		t.Fatalf("REPLICASET outcome = %+v, want the FORBIDDEN failure", replicaSets)
	}
	if record.Reason != string(snapshot.FailureForbidden) {
		t.Errorf("REPLICASET failure reason = %q, want FORBIDDEN", record.Reason)
	}
	if n, observed := replicaSets.Count(); observed {
		t.Errorf("REPLICASET Count() = %d, observed = true; it has no count row at all", n)
	}
}

// 摘要不得落到另一个集群的运行上。
//
// 两个集群的时刻刻意错开：clusterB 的运行更晚，所以任何漏掉
// cluster_id 的取最新查询都会稳定地返回 clusterB 那次，而不是
// 时而返回、时而不返回。CLAUDE.md §4 的"缺 cluster_id 会 join 到
// 错误集群且不报错"，正是这种查得出结果的错。
func TestLatestNeverReturnsAnotherClustersRun(t *testing.T) {
	s, _ := newTestStore(t)

	mustSave(t, s, sampleRun(clusterA, "run-a", runOneAt))

	later := sampleRun(clusterB, "run-b", runTwoAt)
	later.Status = snapshot.RunPartial
	later.Observation.Policies = nil
	later.Failures = []snapshot.Failure{{
		Resource: snapshot.ResourceNetworkPolicy,
		Reason:   snapshot.FailureForbidden,
		Detail:   `networkpolicies.networking.k8s.io is forbidden`,
	}}
	later.Observation.Warnings = []snapshot.Warning{{
		Kind:    snapshot.WarningPodIPOutsideCluster,
		Subject: "shop/web-1",
		Detail:  "10.99.0.4 is outside the registered pod CIDR",
	}}
	mustSave(t, s, later)

	got, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest(%s) error = %v", clusterA, err)
	}
	if got.RunID != "run-a" {
		t.Fatalf("Latest(%s).RunID = %q, want run-a", clusterA, got.RunID)
	}
	if !got.ObservedAt.Equal(runOneAt) {
		t.Errorf("Latest(%s).ObservedAt = %v, want %v", clusterA, got.ObservedAt, runOneAt)
	}

	// clusterB 的失败与告警都不得渗进来。
	for _, o := range got.Resources {
		if record, failed := o.Failure(); failed {
			t.Errorf("%s carries failure %+v; it belongs to %s", o.Resource, record, clusterB)
		}
	}
	if n, observed := observedCount(got.Resources, "NETWORKPOLICY"); !observed || n != 1 {
		t.Errorf("NETWORKPOLICY count = %d (observed=%v), want 1 (%s did see its policy)",
			n, observed, clusterA)
	}
	if got.WarningTotal != 0 {
		t.Errorf("WarningTotal = %d, want 0 (the warning belongs to %s)", got.WarningTotal, clusterB)
	}
}

// marshalResource 把一条资源结果序列化后拆成字段，用于断言"报文里
// 有没有这个键"。走 json.Marshal 而不是直接读结构体：受力的是编码本身。
func marshalResource(t *testing.T, outcome snapshotstore.ResourceOutcome) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(outcome)
	if err != nil {
		t.Fatalf("json.Marshal(%+v) error = %v", outcome, err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", raw, err)
	}
	return fields
}
