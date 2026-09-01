package snapshotstore_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// Service 的暴露信息与 Pod 的命名端口必须原样落库。
//
// 逐字段断言而不是只看非空：一个把 lb_source_ranges 写进 lb_ingress_ips 的
// 实现，在只看非空的测试下照样绿，而它会让 kafka 的放行范围变成三个入口
// 地址。同理 Pod 的命名端口必须连协议一起比对——协议错了，端口解析会
// 认到另一个协议上的同名端口。
func TestServiceExposureAndNamedPortsAreStoredVerbatim(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	run := snapshot.Run{
		Status: snapshot.RunOK, StartedAt: runOneAt, FinishedAt: runOneAt,
		Observation: snapshot.Observation{
			ClusterID: clusterA, RunID: "exposure-run", ObservedAt: runOneAt,
			Services: []snapshot.Service{{
				ClusterID: clusterA, Namespace: "uat-kafka", Name: "kafka-0-external",
				Type: "LoadBalancer", Selector: map[string]string{"app": "kafka"},
				Ports: []snapshot.ServicePort{{
					Name: "tcp-kafka", Port: 9094, TargetPortName: "kafka-external", Protocol: "TCP",
				}},
				LoadBalancerIngressIPs:   []string{"10.170.48.193"},
				LoadBalancerSourceRanges: []string{"10.0.0.0/8", "172.16.0.0/16"},
			}},
			Pods: []snapshot.Pod{{
				ClusterID: clusterA, Namespace: "uat-kafka", Name: "kafka-0",
				UID: "aaaaaaaa-0000-4000-8000-00000000000a", Phase: "Running", IP: "172.16.5.7",
				NamedPorts: []snapshot.NamedPort{{Name: "kafka-external", Port: 9095, Protocol: "TCP"}},
			}},
		},
	}
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var rawIngress, rawRanges string
	if err := db.QueryRowContext(ctx,
		`SELECT lb_ingress_ips, lb_source_ranges FROM observed_service
		  WHERE cluster_id = ? AND name = ?`,
		clusterA, "kafka-0-external").Scan(&rawIngress, &rawRanges); err != nil {
		t.Fatalf("query observed_service: %v", err)
	}
	var gotIngress, gotRanges []string
	if err := json.Unmarshal([]byte(rawIngress), &gotIngress); err != nil {
		t.Fatalf("decode lb_ingress_ips: %v", err)
	}
	if err := json.Unmarshal([]byte(rawRanges), &gotRanges); err != nil {
		t.Fatalf("decode lb_source_ranges: %v", err)
	}
	if !reflect.DeepEqual(gotIngress, []string{"10.170.48.193"}) {
		t.Errorf("lb_ingress_ips = %v, want [10.170.48.193]", gotIngress)
	}
	if !reflect.DeepEqual(gotRanges, []string{"10.0.0.0/8", "172.16.0.0/16"}) {
		t.Errorf("lb_source_ranges = %v, want [10.0.0.0/8 172.16.0.0/16]", gotRanges)
	}

	var rawNamedPorts string
	if err := db.QueryRowContext(ctx,
		`SELECT named_ports FROM observed_pod WHERE cluster_id = ? AND name = ?`,
		clusterA, "kafka-0").Scan(&rawNamedPorts); err != nil {
		t.Fatalf("query observed_pod: %v", err)
	}
	var gotPorts []snapshot.NamedPort
	if err := json.Unmarshal([]byte(rawNamedPorts), &gotPorts); err != nil {
		t.Fatalf("decode named_ports: %v", err)
	}
	want := []snapshot.NamedPort{{Name: "kafka-external", Port: 9095, Protocol: "TCP"}}
	if !reflect.DeepEqual(gotPorts, want) {
		t.Errorf("named_ports = %+v, want %+v", gotPorts, want)
	}
}

// 三列必须真的允许 NULL——钉住 migrations/000032 没有偷偷带 DEFAULT '[]'
// 或 NOT NULL。
//
// 空数组的含义是"采过，是空的"；NULL 的含义是"那次采集根本没有这三列可
// 采"（迁移之前写下的行正是这样）。一条 DEFAULT '[]' 或 NOT NULL 会让这条
// UPDATE 本身报错或被悄悄改写，两者混淆之后推导层会把一批老快照当成
// "这个 LoadBalancer 没有入口地址"，报出一串本不存在的缺口。
func TestPreMigrationRowsKeepTheColumnsNull(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, snapshot.Run{
		Status: snapshot.RunOK, StartedAt: runOneAt, FinishedAt: runOneAt,
		Observation: snapshot.Observation{
			ClusterID: clusterA, RunID: "old-run", ObservedAt: runOneAt,
			Services: []snapshot.Service{{
				ClusterID: clusterA, Namespace: "shop", Name: "api", Type: "ClusterIP",
				Selector: map[string]string{"app": "api"},
			}},
			Pods: []snapshot.Pod{{
				ClusterID: clusterA, Namespace: "shop", Name: "api-1",
				UID: "aaaaaaaa-0000-4000-8000-00000000000b", Phase: "Running", IP: "10.4.0.9",
			}},
		},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// 模拟迁移前写下的行：这次写入其实带了空数组（见上一条用例），
	// 这里手动把它们改回 NULL 来复现"这一列压根不存在时写下的行"。
	if _, err := db.ExecContext(ctx,
		`UPDATE observed_service SET lb_ingress_ips = NULL, lb_source_ranges = NULL
		  WHERE cluster_id = ? AND name = ?`, clusterA, "api"); err != nil {
		t.Fatalf("update service: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE observed_pod SET named_ports = NULL WHERE cluster_id = ? AND name = ?`,
		clusterA, "api-1"); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	var ingressNull, rangesNull, portsNull bool
	if err := db.QueryRowContext(ctx,
		`SELECT lb_ingress_ips IS NULL, lb_source_ranges IS NULL FROM observed_service
		  WHERE cluster_id = ? AND name = ?`, clusterA, "api").Scan(&ingressNull, &rangesNull); err != nil {
		t.Fatalf("query observed_service: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT named_ports IS NULL FROM observed_pod WHERE cluster_id = ? AND name = ?`,
		clusterA, "api-1").Scan(&portsNull); err != nil {
		t.Fatalf("query observed_pod: %v", err)
	}
	if !ingressNull || !rangesNull {
		t.Errorf("lb_ingress_ips IS NULL = %v, lb_source_ranges IS NULL = %v, want both true",
			ingressNull, rangesNull)
	}
	if !portsNull {
		t.Error("named_ports IS NULL = false, want true")
	}
}

// 探针端口逐字节落库。缺了它 KUBELET_PROBE 一条规则都推不出来，而症状
// 要等到下发之后 kubelet 探测被挡、Pod 开始滚动重启才出现。
func TestProbePortsAreStoredVerbatim(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	if err := s.Save(ctx, snapshot.Run{
		Status: snapshot.RunOK, StartedAt: runOneAt, FinishedAt: runOneAt,
		Observation: snapshot.Observation{
			ClusterID: clusterA, RunID: "probe-run", ObservedAt: runOneAt,
			Pods: []snapshot.Pod{{
				ClusterID: clusterA, Namespace: "uat-app", Name: "backend-0",
				UID: "aaaaaaaa-0000-4000-8000-00000000000c", Phase: "Running", IP: "10.4.0.11",
				ProbePorts: []snapshot.NamedPort{
					{Port: 8088, Protocol: "TCP"},
					{Port: 8081, Protocol: "TCP"},
				},
			}},
		},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var raw string
	if err := db.QueryRowContext(ctx,
		`SELECT probe_ports FROM observed_pod WHERE cluster_id = ? AND name = ?`,
		clusterA, "backend-0").Scan(&raw); err != nil {
		t.Fatalf("query observed_pod: %v", err)
	}
	var got []snapshot.NamedPort
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode probe_ports: %v", err)
	}
	want := []snapshot.NamedPort{{Port: 8088, Protocol: "TCP"}, {Port: 8081, Protocol: "TCP"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("probe_ports = %+v, want %+v", got, want)
	}
}

// 采过、但这个 Pod 没有走网络的探针 → 空数组，**不是 NULL**。
//
// 两者的下游含义相反：空数组是「这个 workload 不需要 KUBELET_PROBE」，
// NULL 是「这一行写在这一列存在之前，我们没看过」。写成 NULL 会让每一次
// 正常采集都长得像一次数据缺口。
func TestAPodWithoutProbesStoresAnEmptyArrayNotNull(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	if err := s.Save(ctx, snapshot.Run{
		Status: snapshot.RunOK, StartedAt: runOneAt, FinishedAt: runOneAt,
		Observation: snapshot.Observation{
			ClusterID: clusterA, RunID: "probe-empty-run", ObservedAt: runOneAt,
			Pods: []snapshot.Pod{{
				ClusterID: clusterA, Namespace: "uat-app", Name: "worker-0",
				UID: "aaaaaaaa-0000-4000-8000-00000000000d", Phase: "Running", IP: "10.4.0.12",
			}},
		},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var isNull bool
	var raw string
	if err := db.QueryRowContext(ctx,
		`SELECT probe_ports IS NULL, COALESCE(probe_ports, '') FROM observed_pod
		  WHERE cluster_id = ? AND name = ?`,
		clusterA, "worker-0").Scan(&isNull, &raw); err != nil {
		t.Fatalf("query observed_pod: %v", err)
	}
	if isNull {
		t.Fatal("probe_ports IS NULL —— 一次正常采集被写成了数据缺口的样子")
	}
	if raw != "[]" {
		t.Errorf("probe_ports = %q, want []", raw)
	}
}
