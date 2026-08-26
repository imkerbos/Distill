package collectstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/store"
)

// clusterWithCycle 把测试集群登记成"业务周期已声明"。
//
// 退休建议的三道前提之一。不写就是靠零值碰巧成立，而零值的含义是
// "没有登记过"——那时平台该拒绝给建议。
func clusterWithCycle(t *testing.T, cycle time.Duration) stubSource {
	t.Helper()
	src := testSource()
	for i, c := range src.clusters {
		if c.ID == collectedID {
			c.BusinessCycle = cycle
			c.BusinessCycleReason = "演练集群，分钟级周期"
			src.clusters[i] = c
		}
	}
	return src
}

// **没有观测就不给退休建议。**
//
// 零观测下每一条策略的删除影响都是 0，于是全部显示成"可以退休"——
// 而那个 0 不是评估结果，是没有评估过。这是这个平台最不能给出的那种错觉，
// 而它指向的动作是删掉正在生效的策略。
func TestRetirementRefusesWithoutObservation(t *testing.T) {
	r, s := newTestReaderWithSource(t, clusterWithCycle(t, time.Minute))
	seedPreviewCluster(t, s)
	// 不摄入任何流量。

	got, err := r.Retirement(context.Background(), collectedID, describedWindow())
	if err != nil {
		t.Fatalf("Retirement() = %v", err)
	}
	if got.Eligible {
		t.Error("没有观测却给出了退休建议：每条策略都会显示成「删掉没影响」")
	}
	if len(got.Candidates) != 0 {
		t.Errorf("不给建议时仍然列出了 %d 条候选", len(got.Candidates))
	}
	if got.IneligibleReason == "" {
		t.Error("拒绝了却没说为什么")
	}
}

// **没有登记业务周期同样拒绝。**
//
// 「不知道这个集群多久能看全一轮」比「知道它是七天」更危险：前者连"要不要
// 再等等"这个问题都没有人回答过，而退休一条策略正是最需要那个判断的操作。
func TestRetirementRefusesWithoutABusinessCycle(t *testing.T) {
	r, s := newTestReaderWithSource(t, testSource()) // BusinessCycle 为零值
	seedPreviewCluster(t, s)
	saveIngest(t, s, []flow.Connection{conn(peerIP, recycledIP, portResolved)})

	got, err := r.Retirement(context.Background(), collectedID, describedWindow())
	if err != nil {
		t.Fatalf("Retirement() = %v", err)
	}
	if got.Eligible {
		t.Error("没有登记业务周期却给出了退休建议")
	}
	if got.IneligibleReason != store.RetirementNoBusinessCycle {
		t.Errorf("拒绝理由 = %q，没有指向该去填的那个字段", got.IneligibleReason)
	}
}

// **观测没覆盖一轮业务周期时拒绝。**
//
// 一条只在月结那天走的放行，在这个窗口里看不见，删掉它下个月才会表现出来。
// 与写回那道学习期门槛同源。
func TestRetirementRefusesBeforeAFullBusinessCycle(t *testing.T) {
	// 周期声明成 365 天，而演练的观测只有几十秒。
	r, s := newTestReaderWithSource(t, clusterWithCycle(t, 365*24*time.Hour))
	seedPreviewCluster(t, s)
	saveIngest(t, s, []flow.Connection{conn(peerIP, recycledIP, portResolved)})

	got, err := r.Retirement(context.Background(), collectedID, describedWindow())
	if err != nil {
		t.Fatalf("Retirement() = %v", err)
	}
	if got.Eligible {
		t.Error("观测远没覆盖一轮业务周期，却给出了退休建议")
	}
	if got.IneligibleReason != store.RetirementCycleNotCovered {
		t.Errorf("拒绝理由 = %q", got.IneligibleReason)
	}
}

// 候选集接手了它的职责时，判成可以退休。
//
// **平台自己写的对象不进这份清单**：退休它们走的是写回的删除清单（那条路
// 带确认、带指纹、带审计），而这份报告说的是"集群里那些别人留下的策略
// 还需不需要"。
func TestRetirementMarksACoveredPolicyRetirable(t *testing.T) {
	// 周期设成 1 纳秒：任何一段观测都覆盖得了它，前提因此成立。
	r, s := newTestReaderWithSource(t, clusterWithCycle(t, time.Nanosecond))
	seedClusterWithAllowPolicy(t, s)
	// **完整摄入**：证据可信，那条放行被学成候选规则并默认启用 ——
	// 也就是候选集真的接手了 allow-api 的职责。
	saveIngest(t, s, []flow.Connection{conn(peerIP, recycledIP, portResolved)})

	got, err := r.Retirement(context.Background(), collectedID, describedWindow())
	if err != nil {
		t.Fatalf("Retirement() = %v", err)
	}
	if !got.Eligible {
		t.Fatalf("前提都成立却拒绝了：%s", got.IneligibleReason)
	}
	if len(got.Candidates) == 0 {
		t.Fatal("集群里有两条策略，一条候选都没列出来")
	}

	var target *store.RetirementCandidate
	for i, c := range got.Candidates {
		if c.Name == "allow-api" {
			target = &got.Candidates[i]
		}
		if strings.HasPrefix(c.Name, "candidate-") {
			t.Errorf("平台自己写的对象 %s 进了退休清单 —— 那条路是写回的删除清单", c.Name)
		}
	}
	if target == nil {
		t.Fatalf("没有评估 allow-api：%+v", got.Candidates)
	}
	if !target.Retirable || target.WouldBreak != 0 {
		t.Errorf("候选集已经学到并启用了那条放行，allow-api 却被判成不可退休"+
			"（wouldBreak=%d）", target.WouldBreak)
	}
	if target.CoveredBy == 0 {
		t.Error("判成可以退休，却说没有任何主体接手 —— 那时的 0 只说明" +
			"那些主体在这个窗口里没有流量，不是「候选集覆盖了它」")
	}
}

// **候选集没接手时判成不可退休，并报出它还撑着几条。**
//
// 没有这一条，一个恒答"可以退休"的实现照样能让上面那条通过 ——
// 而那个实现会让操作者删掉正在生效的策略。
func TestRetirementRefusesAPolicyStillHoldingTraffic(t *testing.T) {
	r, s := newTestReaderWithSource(t, clusterWithCycle(t, time.Nanosecond))
	seedClusterWithAllowPolicy(t, s)
	// **明确报告丢过记录**：证据因此全部降级，学出来的规则默认不进启用集，
	// 于是候选集没有接手 allow-api 的职责。
	saveSampledIngest(t, s, []flow.Connection{conn(peerIP, recycledIP, portResolved)})

	got, err := r.Retirement(context.Background(), collectedID, describedWindow())
	if err != nil {
		t.Fatalf("Retirement() = %v", err)
	}
	if !got.Eligible {
		t.Fatalf("前提都成立却拒绝了：%s", got.IneligibleReason)
	}
	var target *store.RetirementCandidate
	for i, c := range got.Candidates {
		if c.Name == "allow-api" {
			target = &got.Candidates[i]
		}
	}
	if target == nil {
		t.Fatalf("没有评估 allow-api：%+v", got.Candidates)
	}
	if target.Retirable {
		t.Error("候选集没有接手那条放行，allow-api 却被判成可以退休 —— " +
			"照做会断掉正在通的连接")
	}
	if target.WouldBreak == 0 {
		t.Error("判成不可退休，却说会断 0 条 —— 两者对不上，操作者无从判断")
	}
}
