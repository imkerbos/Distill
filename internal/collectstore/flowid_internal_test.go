package collectstore

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
)

// ClusterOfFlowID 必须认得**本包自己发出的**那种 ID，并交回它自带的集群。
//
// 内部测试而不是外部包测试：要绑住的正是「导出的那个入口读的是 flowIDOf
// 真的写进去的东西」，而 flowIDOf 是包内的。从包外拿一个手写的 ID 来测，
// 测的是测试自己对格式的复述 —— 编码改一次，测试照常绿，而装配层的下钻
// 分派已经失效。
//
// 分派失效的形态是静默的：ID 认不出来 → 交给 fixture Reader → 那边的数据源
// 里没有这个 COLLECTED 集群 → 答「没有这条」。操作者看到的是一个点不开的
// 链接，而不是任何错误（design doc 2026-08-18 §3.1）。
func TestClusterOfFlowIDReadsBackWhatFlowIDOfWrote(t *testing.T) {
	const clusterID = "kind-distill"
	w := flow.Window{
		From: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 18, 1, 0, 0, 0, time.UTC),
	}
	id := flowIDOf(clusterID, w, 3, flow.Connection{
		Source: flow.Endpoint{IP: "10.4.0.7"},
		Dest:   flow.Endpoint{IP: "10.4.1.9"},
		Port:   8080,
	})

	got, ok := ClusterOfFlowID(id)
	if !ok {
		t.Fatalf("ClusterOfFlowID(%q) not recognised; the assembly layer would hand this "+
			"cluster's drill-down to the fixture reader", id)
	}
	if got != clusterID {
		t.Errorf("ClusterOfFlowID = %q, want %q — dispatching on the wrong cluster reads "+
			"one cluster's connection with another cluster's data", got, clusterID)
	}
}

// 合成数据集的 ID 必须**认不出来**，装配层才会把它交给 fixture Reader。
//
// 反方向同样要钉住：一个把什么都认成「某个集群的 ID」的解码器会让全部下钻
// 都去分派，而合成数据集的 flow-0214 分派不到任何集群，演示环境的下钻会在
// 没有人改动代码的情况下整体点不开。
func TestClusterOfFlowIDRejectsAFixtureShapedID(t *testing.T) {
	for _, id := range []string{"flow-0214", "", "not base64!!"} {
		if got, ok := ClusterOfFlowID(id); ok {
			t.Errorf("ClusterOfFlowID(%q) = %q, true; want it unrecognised so the assembly "+
				"layer falls through to the fixture reader", id, got)
		}
	}
}
