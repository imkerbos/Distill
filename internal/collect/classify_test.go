package collect

import (
	"testing"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// runWithPods 造一次只带 Pod 的采集结果。
func runWithPods(pods ...snapshot.Pod) snapshot.Run {
	return snapshot.Run{Observation: snapshot.Observation{
		ClusterID: testClusterID,
		Pods:      pods,
	}}
}

func TestClassifyFillsScopeForEachPod(t *testing.T) {
	reg := fleetRegistry(t)
	in := runWithPods(
		snapshot.Pod{Namespace: "app", Name: "web-1", IP: "10.0.1.5"},
		snapshot.Pod{Namespace: "kube-system", Name: "agent-1", IP: "192.168.0.10", HostNetwork: true},
	)

	out := Classify(in, reg)

	if got := out.Observation.Pods[0].IPScope; got != cluster.ScopePod {
		t.Errorf("pod IP scope = %q, want %q", got, cluster.ScopePod)
	}
	// hostNetwork Pod 用的就是它所在节点的地址，判成 NODE 是正确答案 ——
	// 不区分的话，一个健康集群里每个 cilium / etcd / kube-proxy 都会各报
	// 一条「落在登记网段之外」。
	if got := out.Observation.Pods[1].IPScope; got != cluster.ScopeNode {
		t.Errorf("hostNetwork pod scope = %q, want %q", got, cluster.ScopeNode)
	}
	// IPScopeReason 按 cluster.Classification 的约定只在 UNKNOWN 时非空
	// （registry.go:92）：判得出来的归属，类别本身就是依据。这里断言它
	// **保持为空**，而不是断言它有值 —— 后者会把一个正确的实现判成错的。
	for i, p := range out.Observation.Pods {
		if p.IPScopeReason != "" {
			t.Errorf("pod[%d] IPScopeReason = %q, want empty for a resolved scope",
				i, p.IPScopeReason)
		}
	}
}

func TestClassifyDoesNotMutateItsInput(t *testing.T) {
	// 入参被改写，「分类前的观测」就没了 —— 而平台侧要能拿原始观测重算
	// 历史（判定依据变了：新集群接入、网段改了），那正是把判定挪到平台
	// 侧的理由（design doc 2026-08-18 §3.4）。
	reg := fleetRegistry(t)
	in := runWithPods(snapshot.Pod{Namespace: "app", Name: "web-1", IP: "10.0.1.5"})

	_ = Classify(in, reg)

	if in.Observation.Pods[0].IPScope != "" {
		t.Errorf("input pod IPScope = %q after Classify, want it untouched",
			in.Observation.Pods[0].IPScope)
	}
	if len(in.Observation.Warnings) != 0 {
		t.Errorf("input warnings = %+v after Classify, want none", in.Observation.Warnings)
	}
}

func TestClassifyCarriesTheWarningsThrough(t *testing.T) {
	reg := fleetRegistry(t)
	in := runWithPods(snapshot.Pod{Namespace: "app", Name: "broken", IP: "not-an-ip"})

	out := Classify(in, reg)

	if len(out.Observation.Warnings) != 1 {
		t.Fatalf("warnings = %+v, want exactly one", out.Observation.Warnings)
	}
	if got := out.Observation.Warnings[0].Kind; got != snapshot.WarningPodIPUnparsable {
		t.Errorf("warning kind = %q, want %q", got, snapshot.WarningPodIPUnparsable)
	}
	if got := out.Observation.Warnings[0].Subject; got != "app/broken" {
		t.Errorf("warning subject = %q, want app/broken — 告警指不出是哪个 Pod，"+
			"运维就只能去猜", got)
	}
}

func TestClassifyKeepsWarningsItWasHandedAndAddsToThem(t *testing.T) {
	// 采集阶段自己产出的告警（比如 workload 解析不出来）必须留着：分类是
	// 追加一层判定，不是重写这次运行的结论。
	reg := fleetRegistry(t)
	in := runWithPods(snapshot.Pod{Namespace: "app", Name: "broken", IP: "not-an-ip"})
	in.Observation.Warnings = []snapshot.Warning{{
		Kind: snapshot.WarningWorkloadUnresolved, Subject: "app/broken",
	}}

	out := Classify(in, reg)

	if len(out.Observation.Warnings) != 2 {
		t.Fatalf("warnings = %+v, want the pre-existing one plus the new one",
			out.Observation.Warnings)
	}
	if out.Observation.Warnings[0].Kind != snapshot.WarningWorkloadUnresolved {
		t.Errorf("the pre-existing warning was dropped: %+v", out.Observation.Warnings)
	}
}

func TestClassifyOverwritesAnyScopeItWasHanded(t *testing.T) {
	// **推送式接入下这份观测来自被管集群里的 agent。** 相信它自己声明的
	// 归属，等于把「这条流量属于哪个集群」这个判断外包给被判断的一方 ——
	// 而归属错了，之后的 join 会落到别的集群的 Pod 上且不报错
	// （CLAUDE.md §4）。所以传进来什么都要重算。
	reg := fleetRegistry(t)
	in := runWithPods(snapshot.Pod{
		Namespace: "app", Name: "web-1", IP: "10.0.1.5",
		IPScope:       cluster.ScopeExternal,
		IPScopeReason: "AGENT_SAID_SO",
	})

	out := Classify(in, reg)

	if got := out.Observation.Pods[0].IPScope; got != cluster.ScopePod {
		t.Errorf("IPScope = %q, want %q — agent 声明的归属存活了下来", got, cluster.ScopePod)
	}
	if got := out.Observation.Pods[0].IPScopeReason; got == "AGENT_SAID_SO" {
		t.Error("IPScopeReason kept the value the agent supplied")
	}
}

func TestClassifyLeavesAPodWithoutAnIPAlone(t *testing.T) {
	reg := fleetRegistry(t)
	in := runWithPods(snapshot.Pod{Namespace: "app", Name: "pending"})

	out := Classify(in, reg)

	if len(out.Observation.Warnings) != 0 {
		t.Errorf("warnings = %+v, want none; Phase already explains an unassigned IP",
			out.Observation.Warnings)
	}
	if out.Observation.Pods[0].IPScope != "" {
		t.Errorf("scope = %q, want empty", out.Observation.Pods[0].IPScope)
	}
}
