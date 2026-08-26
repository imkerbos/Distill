package reconcile_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/replay"
)

// obs 造一条对账输入，端口用来在断言里区分是哪一条。
func obs(ns, wl string, port int32, platform replay.Verdict, observed flow.Verdict) reconcile.Observation {
	return reconcile.Observation{
		Subject:  reconcile.Subject{Namespace: ns, Workload: wl},
		Platform: platform,
		Observed: observed,
		Reported: true,
		Flow: replay.Flow{
			Source:   replay.Endpoint{IP: "10.0.0.1"},
			Dest:     replay.Endpoint{IP: "10.0.0.2"},
			Protocol: replay.ProtocolTCP,
			Port:     port,
		},
	}
}

// 只有两类分歧被抽样。
//
// AGREE 是量最大的一类，而它没有任何可下钻的价值 —— 存下来只会让样本表
// 按流量规模增长（CLAUDE.md §5：这个平台的失控方向是账单）。
// SOURCE_SILENT 与 PLATFORM_UNKNOWN 同理：前者来源什么都没说，后者平台
// 明说不知道，两者都不是"我们算错了"。
func TestOnlyDisagreementsAreSampled(t *testing.T) {
	rep := reconcile.Run([]reconcile.Observation{
		obs("payment", "api", 8080, replay.VerdictAllow, flow.VerdictAllowed), // AGREE
		obs("payment", "api", 8081, replay.VerdictDeny, flow.VerdictAllowed),  // UNDER
		obs("payment", "api", 8082, replay.VerdictAllow, flow.VerdictDenied),  // OVER
		{Subject: reconcile.Subject{Namespace: "payment", Workload: "api"},
			Platform: replay.VerdictUnknown, Observed: flow.VerdictAllowed, Reported: true},
	})

	if len(rep.Samples) != 2 {
		t.Fatalf("样本 %d 条, want 2（只有两类分歧该被抽样）：%+v", len(rep.Samples), rep.Samples)
	}
	for _, s := range rep.Samples {
		if s.Class != reconcile.ClassUnderPermissive && s.Class != reconcile.ClassOverPermissive {
			t.Errorf("抽到了 %s 类的样本，它没有下钻价值，只会让样本表按流量规模增长", s.Class)
		}
	}
}

// 每个 (主体, 类别) 最多留 N 条，且留的是**前 N 条**。
//
// 抽样必须是确定性的：同一批输入两次跑出的样本必须相同，否则一份报告在
// 界面上刷新一次就换一批证据，操作者无从核对。取前 N 条偏向窗口早期，
// 这是已知且可预测的偏差 —— 比一个不可复现的随机取样有用。
func TestSamplesAreCappedPerSubjectAndClass(t *testing.T) {
	var in []reconcile.Observation
	for port := int32(1); port <= 20; port++ {
		in = append(in, obs("payment", "api", port, replay.VerdictDeny, flow.VerdictAllowed))
	}
	// 另一个主体各留各的，不共用一个配额。
	in = append(in, obs("shop", "web", 443, replay.VerdictDeny, flow.VerdictAllowed))

	rep := reconcile.Run(in)

	var apiPorts []int32
	shop := 0
	for _, s := range rep.Samples {
		switch s.Subject.Workload {
		case "api":
			apiPorts = append(apiPorts, s.Flow.Port)
		case "web":
			shop++
		}
	}
	if len(apiPorts) != reconcile.MaxSamplesPerClass {
		t.Fatalf("payment/api 留了 %d 条样本, want %d", len(apiPorts), reconcile.MaxSamplesPerClass)
	}
	for i, p := range apiPorts {
		if p != int32(i+1) {
			t.Errorf("样本 %d 的端口是 %d, want %d —— 抽样不是确定性的前 N 条", i, p, i+1)
		}
	}
	if shop != 1 {
		t.Errorf("shop/web 留了 %d 条, want 1 —— 各主体的配额被共用了", shop)
	}
}

// 样本按 (主体, 类别) 稳定排序，与 BySubject 同一条理由。
func TestSamplesAreOrderedStably(t *testing.T) {
	in := []reconcile.Observation{
		obs("shop", "web", 443, replay.VerdictDeny, flow.VerdictAllowed),
		obs("payment", "api", 8080, replay.VerdictAllow, flow.VerdictDenied),
		obs("payment", "api", 8081, replay.VerdictDeny, flow.VerdictAllowed),
	}
	first := reconcile.Run(in).Samples
	for range 5 {
		got := reconcile.Run(in).Samples
		for i := range got {
			if got[i].Subject != first[i].Subject || got[i].Class != first[i].Class ||
				got[i].Flow.Port != first[i].Flow.Port {
				t.Fatalf("同一批输入两次跑出的样本次序不同：%+v vs %+v", got, first)
			}
		}
	}
	// UNDER_PERMISSIVE 排在 OVER_PERMISSIVE 之前：它是唯一能造成生产阻断的
	// 那一类，翻页翻不到的证据等于没有。
	if first[0].Class != reconcile.ClassUnderPermissive {
		t.Errorf("首条样本是 %s，想要的是把能造成阻断的那一类排在前面", first[0].Class)
	}
}
