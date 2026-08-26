package snapshotstore_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// podRun 是一次只关心 Pod 的采集运行：其余资源都采到了，条数为 0。
//
// 不复用 sampleRun：那一份每类资源各一条，而这一组测试要断言的是区间表的
// 行数，多出来的 Service 与 Endpoints 只会让失败信息更难读。
func podRun(clusterID, runID string, observedAt time.Time, pods ...snapshot.Pod) snapshot.Run {
	return snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  observedAt.Add(-time.Second),
		FinishedAt: observedAt,
		Observation: snapshot.Observation{
			ClusterID: clusterID, RunID: runID, ObservedAt: observedAt, Pods: pods,
		},
	}
}

// derive 推导一次运行，失败即 t.Fatal。
func derive(t *testing.T, s *snapshotstore.Store, clusterID, runID string) {
	t.Helper()
	if err := s.DeriveIdentityIntervals(t.Context(), clusterID, runID); err != nil {
		t.Fatalf("DeriveIdentityIntervals(%s, %s) error = %v", clusterID, runID, err)
	}
}

// dumpIntervals 把区间表整表读成一组可比较的字符串。
//
// 逐列读出而非 SELECT COUNT(*)：幂等要求的是"逐字节相同"，而只数行数的
// 断言在一个把 last_run_id 每次都改掉的实现下照样是绿的。
func dumpIntervals(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT cluster_id, pod_ip, valid_from, valid_to, namespace, pod_name, pod_uid,
		        workload_kind, workload_name, host_network, in_mesh,
		        first_run_id, last_run_id, closed_reason
		   FROM pod_identity_interval
		  ORDER BY cluster_id, pod_ip, valid_from`)
	if err != nil {
		t.Fatalf("dump intervals: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var (
			clusterID, podIP, ns, podName, podUID string
			workloadKind, workloadName            string
			firstRunID, lastRunID, closedReason   string
			hostNetwork, inMesh                   bool
			validFrom                             time.Time
			validTo                               sql.NullTime
		)
		if err := rows.Scan(&clusterID, &podIP, &validFrom, &validTo, &ns, &podName, &podUID,
			&workloadKind, &workloadName, &hostNetwork, &inMesh,
			&firstRunID, &lastRunID, &closedReason); err != nil {
			t.Fatalf("scan interval: %v", err)
		}
		end := "OPEN"
		if validTo.Valid {
			end = validTo.Time.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, fmt.Sprintf("%s %s [%s,%s) %s/%s uid=%s %s/%s host=%t mesh=%t first=%s last=%s closed=%q",
			clusterID, podIP, validFrom.UTC().Format(time.RFC3339Nano), end,
			ns, podName, podUID, workloadKind, workloadName, hostNetwork, inMesh,
			firstRunID, lastRunID, closedReason))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate intervals: %v", err)
	}
	return out
}

func intervalCount(t *testing.T, db *sql.DB, clusterID string) int {
	t.Helper()
	return scanInt(t, db,
		`SELECT COUNT(*) FROM pod_identity_interval WHERE cluster_id = ?`, clusterID)
}

// 推导幂等：对同一次运行重跑，区间表逐行相同，不产生重复行。
//
// 重试、补跑与并发都会让同一批输入被推导多次（spec §2 / §6）。这里两次
// 运行各推导两遍，且第二遍夹在第一次运行的两遍之后 —— 只重跑一次运行的
// 测试，在一个"每次都追加"的实现下也可能因为主键冲突而报错通过，
// 而这里断言的是表的内容本身没有变。
func TestDerivingTwiceLeavesTheSameRows(t *testing.T) {
	s, db := newTestStore(t)

	mustSave(t, s, podRun(clusterA, "run-1", runOneAt, samplePod(clusterA, "web-1", "10.4.0.9")))
	derive(t, s, clusterA, "run-1")
	first := dumpIntervals(t, db)
	if len(first) != 1 {
		t.Fatalf("after the first derivation the table holds %d rows, want 1: %v", len(first), first)
	}

	derive(t, s, clusterA, "run-1")
	if again := dumpIntervals(t, db); !slices.Equal(first, again) {
		t.Errorf("deriving run-1 twice changed the table:\nfirst  = %v\nsecond = %v", first, again)
	}

	// 第二次运行让这个 Pod 消失，于是区间被关上；关闭这一步同样要幂等。
	mustSave(t, s, podRun(clusterA, "run-2", runTwoAt))
	derive(t, s, clusterA, "run-2")
	closed := dumpIntervals(t, db)

	derive(t, s, clusterA, "run-2")
	if again := dumpIntervals(t, db); !slices.Equal(closed, again) {
		t.Errorf("deriving run-2 twice changed the table:\nfirst  = %v\nsecond = %v", closed, again)
	}
	// 再回头重推第一次运行也不得多出一行。
	derive(t, s, clusterA, "run-1")
	if again := dumpIntervals(t, db); !slices.Equal(closed, again) {
		t.Errorf("re-deriving run-1 after run-2 changed the table:\nbefore = %v\nafter  = %v", closed, again)
	}
}

// 关闭区间必须记下是哪一次运行关的，事后能追（spec §3 推论）。
//
// 一个区间被关掉意味着平台认定这个工作负载下线了，于是它的策略会被判成
// "无流量、可收紧"。事后必须能追到那个结论是哪一次采集下的，才能去查那次
// 采集可不可信 —— 否则一次权限故障留下的痕迹与一次真的下线完全一样。
func TestClosingRecordsWhichRunClosedIt(t *testing.T) {
	s, db := newTestStore(t)

	mustSave(t, s, podRun(clusterA, "run-open", runOneAt, samplePod(clusterA, "web-1", "10.4.0.9")))
	derive(t, s, clusterA, "run-open")

	mustSave(t, s, podRun(clusterA, "run-close", runTwoAt))
	derive(t, s, clusterA, "run-close")

	var (
		validTo      sql.NullTime
		firstRunID   string
		lastRunID    string
		closedReason string
	)
	if err := db.QueryRow(
		`SELECT valid_to, first_run_id, last_run_id, closed_reason
		   FROM pod_identity_interval
		  WHERE cluster_id = ? AND pod_ip = ? AND valid_from = ?`,
		clusterA, "10.4.0.9", runOneAt).
		Scan(&validTo, &firstRunID, &lastRunID, &closedReason); err != nil {
		t.Fatalf("read the closed interval: %v", err)
	}

	if !validTo.Valid || !validTo.Time.UTC().Equal(runTwoAt) {
		t.Errorf("valid_to = %v (valid=%t), want %v", validTo.Time, validTo.Valid, runTwoAt)
	}
	if lastRunID != "run-close" {
		t.Errorf("last_run_id = %q, want run-close: the run that concluded the pod was gone "+
			"must be recoverable, otherwise nobody can check whether it was trustworthy", lastRunID)
	}
	if firstRunID != "run-open" {
		t.Errorf("first_run_id = %q, want run-open: closing must not rewrite who opened it", firstRunID)
	}
	if closedReason != string(identity.ClosedAbsentInLaterRun) {
		t.Errorf("closed_reason = %q, want %q", closedReason, identity.ClosedAbsentInLaterRun)
	}
}

// 一个稳定的 Pod 无论被采集多少次，只有一行区间 —— 这正是引入区间表的
// 收益（spec §7）。
//
// observed_pod 随采集次数线性增长，区间表按 Pod 生命周期次数增长。
// 一个长命 Pod 每次采集攒一行，这张表就没有存在的理由。
func TestAStablePodStaysOneRow(t *testing.T) {
	s, db := newTestStore(t)

	pod := samplePod(clusterA, "web-1", "10.4.0.9")
	runs := []struct {
		id string
		at time.Time
	}{
		{"run-1", runOneAt},
		{"run-2", runTwoAt},
		{"run-3", runTwoAt.Add(10 * time.Minute)},
		{"run-4", runTwoAt.Add(20 * time.Minute)},
	}
	for _, r := range runs {
		mustSave(t, s, podRun(clusterA, r.id, r.at, pod))
		derive(t, s, clusterA, r.id)
	}

	// 快照那边确实攒了四行 —— 否则这条断言可能只是因为压根没采到东西。
	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM observed_pod WHERE cluster_id = ? AND name = 'web-1'`,
		clusterA); n != len(runs) {
		t.Fatalf("observed_pod rows = %d, want %d (one per collection)", n, len(runs))
	}
	if n := intervalCount(t, db, clusterA); n != 1 {
		t.Fatalf("pod_identity_interval rows for a pod seen %d times = %d, want 1: %v",
			len(runs), n, dumpIntervals(t, db))
	}

	// 那一行还得是开着的，并记着最后一次看见它的运行。
	var (
		validTo   sql.NullTime
		lastRunID string
	)
	if err := db.QueryRow(
		`SELECT valid_to, last_run_id FROM pod_identity_interval WHERE cluster_id = ?`, clusterA).
		Scan(&validTo, &lastRunID); err != nil {
		t.Fatalf("read the interval: %v", err)
	}
	if validTo.Valid {
		t.Errorf("valid_to = %v, want NULL: the pod is still there", validTo.Time)
	}
	if lastRunID != "run-4" {
		t.Errorf("last_run_id = %q, want run-4 (the most recent run that saw it)", lastRunID)
	}
}

// Pod 这一类没采到时，一个区间都不许关 —— 哪怕整次运行报的是 OK。
//
// 这是 spec §3 那条守卫在存储层的落点。identity 包已经测过守卫本身，
// 但那证明不了调用方还在按它的口径喂参数：这里断言的是 PodsCollected 真的
// 来自 collection_run_failure，而不是被一个"运行是 OK 就当成采到了"的
// 实现填成了 true。把 FORBIDDEN 读成"Pod 没了"，会让一次权限故障变成
// 一批工作负载被判定下线。
func TestAPodCollectionFailureClosesNothingEvenWhenTheRunSaysOK(t *testing.T) {
	s, db := newTestStore(t)

	mustSave(t, s, podRun(clusterA, "run-open", runOneAt, samplePod(clusterA, "web-1", "10.4.0.9")))
	derive(t, s, clusterA, "run-open")

	// 状态报 OK，但 Pod 这一类带着一条失败记录：区间表看的必须是后者。
	blind := podRun(clusterA, "run-blind", runTwoAt)
	blind.Failures = []snapshot.Failure{{
		Resource: snapshot.ResourcePod,
		Reason:   snapshot.FailureForbidden,
		Detail:   "pods is forbidden",
	}}
	mustSave(t, s, blind)
	derive(t, s, clusterA, "run-blind")

	var validTo sql.NullTime
	if err := db.QueryRow(
		`SELECT valid_to FROM pod_identity_interval WHERE cluster_id = ?`, clusterA).
		Scan(&validTo); err != nil {
		t.Fatalf("read the interval: %v", err)
	}
	if validTo.Valid {
		t.Errorf("valid_to = %v, want NULL: a run that could not read pods must never be taken "+
			"for those pods having disappeared", validTo.Time)
	}
}

// 落库的每一个运行状态都必须被显式映射，没登记的一律拒绝推导。
//
// 三个取值各跑一遍而不是只跑一个：identity.RunStatus 与 snapshot.RunStatus
// 是两个同值的封闭枚举，一次强制转换在今天是对的，而它取消了两者之间唯一的
// 对账点。这条从落库的字符串一路走到"关不关区间"，因此一个把映射删成
// 强制转换的改动，会在库里出现一个未登记状态的那一刻被这里挡住。
func TestEveryStoredRunStatusIsMappedNotCast(t *testing.T) {
	cases := []struct {
		stored     string
		wantClosed bool
		wantErr    bool
	}{
		{string(snapshot.RunOK), true, false},
		{string(snapshot.RunPartial), false, false},
		{string(snapshot.RunFailed), false, false},
		{"DEGRADED", false, true}, // 未登记：拒绝推导，不猜
	}

	for _, c := range cases {
		t.Run(c.stored, func(t *testing.T) {
			s, db := newTestStore(t)

			mustSave(t, s, podRun(clusterA, "run-open", runOneAt, samplePod(clusterA, "web-1", "10.4.0.9")))
			derive(t, s, clusterA, "run-open")

			// 第二次运行看不到这个 Pod；它的状态是本例要检验的那个。
			mustSave(t, s, podRun(clusterA, "run-next", runTwoAt))
			if _, err := db.Exec(
				`UPDATE collection_run SET status = ? WHERE cluster_id = ? AND run_id = ?`,
				c.stored, clusterA, "run-next"); err != nil {
				t.Fatalf("set status %q: %v", c.stored, err)
			}

			err := s.DeriveIdentityIntervals(t.Context(), clusterA, "run-next")
			if c.wantErr {
				if err == nil {
					t.Fatalf("DeriveIdentityIntervals() = nil error for status %q, "+
						"want it refused: an unregistered status cannot be judged", c.stored)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeriveIdentityIntervals() error = %v", err)
			}

			var validTo sql.NullTime
			if err := db.QueryRow(
				`SELECT valid_to FROM pod_identity_interval WHERE cluster_id = ?`, clusterA).
				Scan(&validTo); err != nil {
				t.Fatalf("read the interval: %v", err)
			}
			if validTo.Valid != c.wantClosed {
				t.Errorf("interval closed = %t for status %q, want %t",
					validTo.Valid, c.stored, c.wantClosed)
			}
		})
	}
}

// 归属退化不得让同一个 Pod 在表里多出一行（spec §2）。
//
// ReplicaSet 这一类采不到时（一次 PARTIAL 运行），Pod 的归属从 Deployment/web
// 退化成 ReplicaSet/web-6d4f。区间的主体是 pod_uid，不是那两个标签：多出来的
// 那一行会让这个地址从此答 AMBIGUOUS，而权限不恢复时每次运行都是 PARTIAL，
// 没有一次运行有资格关掉它们 —— 一次 RBAC 缺口伪装成"采集间隔太长"。
//
// 走库而不只走 identity 包：订正落在 UPDATE 的列清单上，那份清单只有真的
// 发到 MySQL 才算证到。
func TestADegradedWorkloadLabelDoesNotForkAPodInTheTable(t *testing.T) {
	s, db := newTestStore(t)

	degraded := samplePod(clusterA, "web-1", "10.4.0.9")
	degraded.WorkloadKind = "ReplicaSet"
	degraded.WorkloadName = "web-6d4f"

	blind := podRun(clusterA, "run-blind", runOneAt, degraded)
	blind.Status = snapshot.RunPartial
	blind.Failures = []snapshot.Failure{{
		Resource: snapshot.ResourceReplicaSet,
		Reason:   snapshot.FailureForbidden,
		Detail:   "replicasets is forbidden",
	}}
	mustSave(t, s, blind)
	derive(t, s, clusterA, "run-blind")

	// 权限恢复：同一个 Pod，归属解回 Deployment/web。
	mustSave(t, s, podRun(clusterA, "run-ok", runTwoAt, samplePod(clusterA, "web-1", "10.4.0.9")))
	derive(t, s, clusterA, "run-ok")

	if n := intervalCount(t, db, clusterA); n != 1 {
		t.Fatalf("a pod whose workload label degraded holds %d interval rows, want 1: %v",
			n, dumpIntervals(t, db))
	}

	var (
		validFrom             time.Time
		validTo               sql.NullTime
		kind, name            string
		firstRunID, lastRunID string
	)
	if err := db.QueryRow(
		`SELECT valid_from, valid_to, workload_kind, workload_name, first_run_id, last_run_id
		   FROM pod_identity_interval WHERE cluster_id = ?`, clusterA).
		Scan(&validFrom, &validTo, &kind, &name, &firstRunID, &lastRunID); err != nil {
		t.Fatalf("read the interval: %v", err)
	}
	if !validFrom.UTC().Equal(runOneAt) || validTo.Valid || firstRunID != "run-blind" {
		t.Errorf("interval = [%v,%v) first=%s, want the original one still open from %v",
			validFrom, validTo.Time, firstRunID, runOneAt)
	}
	if kind != "Deployment" || name != "web" {
		t.Errorf("workload = %s/%s, want Deployment/web: a trustworthy run resolved it, and leaving "+
			"the RBAC gap's answer in place would pin this pod to a ReplicaSet for its whole life",
			kind, name)
	}
	if lastRunID != "run-ok" {
		t.Errorf("last_run_id = %q, want run-ok", lastRunID)
	}
}

// hostNetworkPod 是一个跑在节点地址上的 Pod，UID 各不相同。
//
// UID 必须显式给：samplePod 对所有 Pod 用同一个 UID，而 pod_uid 现在在主键里，
// 沿用会让这组测试"通过"的原因变成三个 Pod 其实是同一行。
func hostNetworkPod(clusterID, name, uid, nodeIP string) snapshot.Pod {
	p := samplePod(clusterID, name, nodeIP)
	p.Namespace = "kube-system"
	p.UID = uid
	p.HostNetwork = true
	p.WorkloadKind = "DaemonSet"
	p.WorkloadName = name
	return p
}

// loadIntervals 读出一个 (cluster_id, pod_ip) 的全部区间，喂给解析器。
//
// 只存在于测试里：本轮不给区间表接生产读侧（spec §9）。但"表放得下重叠"
// 这件事只有走到 identity.Resolve 才算证到 —— 几行并排躺在表里却没有人读，
// 与它们根本不存在是同一个结果。
func loadIntervals(t *testing.T, db *sql.DB, clusterID, podIP string) []identity.Interval {
	t.Helper()
	rows, err := db.Query(
		`SELECT valid_from, valid_to, namespace, pod_name, pod_uid,
		        workload_kind, workload_name, host_network, in_mesh,
		        first_run_id, last_run_id, closed_reason
		   FROM pod_identity_interval
		  WHERE cluster_id = ? AND pod_ip = ?`, clusterID, podIP)
	if err != nil {
		t.Fatalf("load intervals: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []identity.Interval
	for rows.Next() {
		var (
			iv           = identity.Interval{ClusterID: clusterID, PodIP: podIP}
			validTo      sql.NullTime
			closedReason string
		)
		if err := rows.Scan(&iv.ValidFrom, &validTo, &iv.Identity.Namespace, &iv.Identity.PodName,
			&iv.Identity.PodUID, &iv.Identity.WorkloadKind, &iv.Identity.WorkloadName,
			&iv.Identity.HostNetwork, &iv.Identity.InMesh,
			&iv.FirstRunID, &iv.LastRunID, &closedReason); err != nil {
			t.Fatalf("scan interval: %v", err)
		}
		if validTo.Valid {
			iv.ValidTo = validTo.Time.UTC()
		}
		iv.ClosedReason = identity.ClosedReason(closedReason)
		out = append(out, iv)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate intervals: %v", err)
	}
	return out
}

// 同一个节点上的多个 hostNetwork Pod 共用节点地址，这是常态不是数据故障
// （spec §2 / §4）。三行都必须留下，且那一刻的解析结果是 AMBIGUOUS。
//
// 两半都是必需的：
//
//   - 三行都留下。主键里没有 pod_uid 时，这张表只放得下其中一个 —— 剩下那些
//     主体在这段时间里彻底查不到，而查得到的那个仍然答得出、仍然不报错。
//   - 解析结果是 AMBIGUOUS 而不是其中之一。行并排躺着却没人读，与它们根本
//     不存在是同一个结果；这里从库里读回来走一遍 identity.Resolve，
//     断言它按 spec §4 拒绝任选。
//
// 排除 hostNetwork Pod 不是替代方案：那样解析器对一个节点地址会答
// NOT_COVERED，意思是"那一刻没有 Pod 用这个地址"—— 而事实是有好几个。
func TestHostNetworkPodsShareANodeAddressAndResolveAmbiguous(t *testing.T) {
	s, db := newTestStore(t)

	const nodeIP = "10.128.0.5"
	agents := []snapshot.Pod{
		hostNetworkPod(clusterA, "cilium-agent", "aaaaaaaa-0000-4000-8000-00000000000a", nodeIP),
		hostNetworkPod(clusterA, "fluent-bit", "bbbbbbbb-0000-4000-8000-00000000000b", nodeIP),
		hostNetworkPod(clusterA, "node-exporter", "cccccccc-0000-4000-8000-00000000000c", nodeIP),
	}
	// 一个普通 Pod 一起落库：少了它，一个"什么都答 AMBIGUOUS"的实现照样全绿。
	pods := append(agents, samplePod(clusterA, "web-1", "10.4.0.9"))

	mustSave(t, s, podRun(clusterA, "run-1", runOneAt, pods...))
	derive(t, s, clusterA, "run-1")

	shared := loadIntervals(t, db, clusterA, nodeIP)
	if len(shared) != len(agents) {
		t.Fatalf("intervals for the node address = %d, want %d (one per hostNetwork pod): %v",
			len(shared), len(agents), dumpIntervals(t, db))
	}
	// 三行是三个不同的主体，不是同一个被写了三遍。
	uids := map[string]bool{}
	for _, iv := range shared {
		uids[iv.Identity.PodUID] = true
	}
	if len(uids) != len(agents) {
		t.Errorf("distinct pod uids on the node address = %d, want %d: %v",
			len(uids), len(agents), dumpIntervals(t, db))
	}

	// 那一刻的解析必须是 AMBIGUOUS，且不交出任何一个主体。
	got, outcome := identity.Resolve(shared, runOneAt)
	if outcome != identity.OutcomeAmbiguous {
		t.Errorf("Resolve(node address) outcome = %q, want %q: several subjects held it at that "+
			"moment, and picking one still answers, still does not error",
			outcome, identity.OutcomeAmbiguous)
	}
	if got != (identity.Identity{}) {
		t.Errorf("Resolve(node address) identity = %+v, want the zero value", got)
	}

	// 同一批数据里的普通 Pod 地址仍然解得出来 —— 上面那条不是靠"全都 AMBIGUOUS"通过的。
	if _, outcome := identity.Resolve(loadIntervals(t, db, clusterA, "10.4.0.9"), runOneAt); outcome != identity.OutcomeResolved {
		t.Errorf("Resolve(10.4.0.9) outcome = %q, want %q", outcome, identity.OutcomeResolved)
	}
}

// 一个集群的推导不得碰另一个集群的区间。
//
// 不同集群的 Pod CIDR 可以重叠（CLAUDE.md §4）。让 A 的运行去关 B 的区间，
// 正是"join 到错误的 Pod 上且不报错"的那种错法。
func TestDerivingOneClusterLeavesTheOtherAlone(t *testing.T) {
	s, db := newTestStore(t)

	mustSave(t, s, podRun(clusterA, "run-a", runOneAt, samplePod(clusterA, "web-1", "10.4.0.9")))
	mustSave(t, s, podRun(clusterB, "run-b", runOneAt, samplePod(clusterB, "web-1", "10.4.0.9")))
	derive(t, s, clusterA, "run-a")
	derive(t, s, clusterB, "run-b")

	// B 的 Pod 消失了，A 的没有。
	mustSave(t, s, podRun(clusterB, "run-b2", runTwoAt))
	derive(t, s, clusterB, "run-b2")

	var validTo sql.NullTime
	if err := db.QueryRow(
		`SELECT valid_to FROM pod_identity_interval WHERE cluster_id = ? AND pod_ip = ?`,
		clusterA, "10.4.0.9").Scan(&validTo); err != nil {
		t.Fatalf("read cluster A interval: %v", err)
	}
	if validTo.Valid {
		t.Errorf("cluster A interval closed at %v by a run of cluster B", validTo.Time)
	}
}

// **双栈 Pod 的两个地址各建一条区间。**
//
// identity.Derive 按 (地址, 主体) 建区间，因此第二个地址只有被展开成独立的
// 一条观测，区间表里才会有它。少了它，走那个地址的连接解不出主体、判
// UNKNOWN，覆盖它的规则于是缺席 —— 下发 default-deny 之后会被拦断。
func TestDualStackPodGetsAnIntervalPerAddress(t *testing.T) {
	s, db := newTestStore(t)
	at := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	pod := snapshot.Pod{
		ClusterID: clusterA, Namespace: "payment", Name: "api-1",
		UID: "3c9d2b1a-0000-4000-8000-0000000000d1", Phase: "Running",
		IP:           "10.4.1.7",
		ExtraIPs:     []snapshot.PodAddress{{IP: "fd00:10:4::7"}},
		Labels:       map[string]string{"app": "api"},
		WorkloadKind: "Deployment", WorkloadName: "api",
	}
	if err := s.Save(t.Context(), podRun(clusterA, "dual-run-000000000000000001", at, pod)); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	derive(t, s, clusterA, "dual-run-000000000000000001")

	got := dumpIntervals(t, db)
	if len(got) != 2 {
		t.Fatalf("建了 %d 条区间, want 2（每个地址一条）：\n%s",
			len(got), strings.Join(got, "\n"))
	}
	var sawV4, sawV6 bool
	for _, row := range got {
		if strings.Contains(row, "10.4.1.7") {
			sawV4 = true
		}
		if strings.Contains(row, "fd00:10:4::7") {
			sawV6 = true
		}
		// 两条区间指向同一个主体：它们是同一个 Pod 的两个地址。
		if !strings.Contains(row, "payment") || !strings.Contains(row, "api-1") {
			t.Errorf("区间没挂到 payment/api-1 上：%s", row)
		}
	}
	if !sawV4 || !sawV6 {
		t.Errorf("缺了协议族（v4=%v v6=%v）：\n%s", sawV4, sawV6, strings.Join(got, "\n"))
	}
}

// 单栈 Pod 仍然只有一条区间：绝大多数集群走这条路，形状不能变。
func TestSingleStackPodStillGetsOneInterval(t *testing.T) {
	s, db := newTestStore(t)
	at := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)

	pod := snapshot.Pod{
		ClusterID: clusterA, Namespace: "payment", Name: "api-1",
		UID: "3c9d2b1a-0000-4000-8000-0000000000d2", Phase: "Running",
		IP: "10.4.1.7", Labels: map[string]string{"app": "api"},
		WorkloadKind: "Deployment", WorkloadName: "api",
	}
	if err := s.Save(t.Context(), podRun(clusterA, "single-run-00000000000000001", at, pod)); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	derive(t, s, clusterA, "single-run-00000000000000001")

	if got := dumpIntervals(t, db); len(got) != 1 {
		t.Errorf("单栈 Pod 建了 %d 条区间, want 1：\n%s", len(got), strings.Join(got, "\n"))
	}
}
