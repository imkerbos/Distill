package reconcile_test

import (
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/replay"
)

// 五种关系各自落到自己那一档（design doc 2026-08-25 §3.2）。
//
// 两个方向的分歧必须分开：它们的危险性完全不对称，合并成一个"分歧率"
// 会让唯一能造成生产阻断的那一类被稀释。
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform replay.Verdict
		observed flow.Verdict
		reported bool
		want     reconcile.Class
	}{
		{"两边都放行", replay.VerdictAllow, flow.VerdictAllowed, true, reconcile.ClassAgree},
		{"两边都拦下", replay.VerdictDeny, flow.VerdictDenied, true, reconcile.ClassAgree},
		{"来源没报判定", replay.VerdictAllow, "", false, reconcile.ClassSourceSilent},
		{"平台答不出", replay.VerdictUnknown, flow.VerdictAllowed, true, reconcile.ClassPlatformUnknown},
		{"平台高估放行面", replay.VerdictAllow, flow.VerdictDenied, true, reconcile.ClassOverPermissive},
		{"平台低估放行面", replay.VerdictDeny, flow.VerdictAllowed, true, reconcile.ClassUnderPermissive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconcile.Classify(tc.platform, tc.observed, tc.reported); got != tc.want {
				t.Errorf("Classify() = %q, want %q", got, tc.want)
			}
		})
	}
}

// **来源没报判定先于一切其它判断。**
//
// 空判定与"报了放行"是两件事。少了这一条，一个 conntrack 接入的集群
// （恒不报判定）会把每一条平台判 DENY 的连接算成分歧，于是一致率归零，
// 而引擎的正确性一点没变。
func TestSilentSourceNeverCountsAsDisagreement(t *testing.T) {
	for _, v := range []replay.Verdict{replay.VerdictAllow, replay.VerdictDeny, replay.VerdictUnknown} {
		if got := reconcile.Classify(v, "", false); got != reconcile.ClassSourceSilent {
			t.Errorf("平台判 %s、来源没报 → %q, want SOURCE_SILENT", v, got)
		}
	}
}

// 平台给出一个未登记的判定时，不得算成一致。
//
// 那是把一个说不清的东西计入分子 —— 一致率于是虚高，而虚高的可信度指标
// 比没有指标更糟。
func TestAnUnregisteredPlatformVerdictIsNotAgreement(t *testing.T) {
	if got := reconcile.Classify(replay.Verdict("SOMETHING_NEW"), flow.VerdictAllowed, true); got == reconcile.ClassAgree {
		t.Error("一个平台不认识的判定被算成了一致")
	}
}

// 一致率的分母只含可比对的那三类（§3.2）。
func TestAgreementRateExcludesSilentAndUnknown(t *testing.T) {
	c := reconcile.Counts{
		reconcile.ClassAgree:           8,
		reconcile.ClassOverPermissive:  1,
		reconcile.ClassUnderPermissive: 1,
		// 下面两类不进分母，各给一个大数：进了分母，一致率会被压到 0.1 以下。
		reconcile.ClassSourceSilent:    500,
		reconcile.ClassPlatformUnknown: 500,
	}
	rate, ok := c.AgreementRate()
	if !ok {
		t.Fatal("有可比对的连接，却答不出一致率")
	}
	if rate != 0.8 {
		t.Errorf("AgreementRate() = %v, want 0.8 —— 分母混进了没报判定或平台答不出的那两类", rate)
	}
	under, ok := c.UnderPermissiveRate()
	if !ok || under != 0.1 {
		t.Errorf("UnderPermissiveRate() = %v (%v), want 0.1", under, ok)
	}
}

// **没有可比对的连接时，一致率答不出来，而不是 0 或 1。**
//
// 返回 0 会被读成"全错"，返回 1 会被读成"全对"，两者都是编的。这与平台
// 其余地方"答不出就说答不出"是同一条纪律。
func TestNoComparableConnectionsMeansNoRate(t *testing.T) {
	c := reconcile.Counts{reconcile.ClassSourceSilent: 100, reconcile.ClassPlatformUnknown: 50}
	if rate, ok := c.AgreementRate(); ok {
		t.Errorf("AgreementRate() = %v, true —— 一条可比对的连接都没有，不该给出数字", rate)
	}
}

// 聚合按 workload 分组：整集群平均值会把一个全错的 workload 藏进
// 几千条正确判定里，而门禁正是按 workload 拦的。
func TestRunAggregatesPerWorkload(t *testing.T) {
	bad := reconcile.Subject{Namespace: "payment", Workload: "api"}
	good := reconcile.Subject{Namespace: "shop", Workload: "web"}
	in := []reconcile.Observation{
		{Subject: bad, Platform: replay.VerdictDeny, Observed: flow.VerdictAllowed, Reported: true},
		{Subject: bad, Platform: replay.VerdictDeny, Observed: flow.VerdictAllowed, Reported: true},
	}
	for range 20 {
		in = append(in, reconcile.Observation{
			Subject: good, Platform: replay.VerdictAllow, Observed: flow.VerdictAllowed, Reported: true,
		})
	}

	rep := reconcile.Run(in)
	if rep.Total != 22 {
		t.Errorf("Total = %d, want 22", rep.Total)
	}
	// 整集群一致率被大量正确判定抬高，而那个坏 workload 是 0。
	overall, _ := rep.Overall.AgreementRate()
	if overall < 0.9 {
		t.Errorf("整集群一致率 = %v, 期望被正确判定抬高（这正是不能只看整集群的理由）", overall)
	}
	var found bool
	for _, s := range rep.BySubject {
		if s.Subject != bad {
			continue
		}
		found = true
		if rate, ok := s.Counts.AgreementRate(); !ok || rate != 0 {
			t.Errorf("payment/api 的一致率 = %v (%v), want 0", rate, ok)
		}
		if under, _ := s.Counts.UnderPermissiveRate(); under != 1 {
			t.Errorf("payment/api 的低估率 = %v, want 1 —— 这一类是会造成阻断的那个方向", under)
		}
	}
	if !found {
		t.Fatal("按 workload 的聚合里没有 payment/api")
	}
	// 输出顺序稳定：随 map 遍历顺序变化的清单会让同一批数据每次读起来都不一样。
	if len(rep.BySubject) != 2 || rep.BySubject[0].Subject != bad {
		t.Errorf("BySubject 顺序不稳定或分组错了: %+v", rep.BySubject)
	}
}

// 没有 workload 归属标签的主体，文案必须说清为什么，而不是渲染成断尾。
//
// 门禁的拒绝理由要点名一个操作者能据以行动的东西。"probe-manual/" 后面
// 那个空白，读起来像是文案坏了 —— 而实际情况是那些 Pod 一个归属标签都没有，
// 它本身就是要被修的东西。
func TestASubjectWithoutAWorkloadLabelSaysWhy(t *testing.T) {
	s := reconcile.Subject{Namespace: "probe-manual"}
	label := s.Label()
	if strings.HasSuffix(label, "/") {
		t.Errorf("Label() = %q —— 断尾的名字操作者拿不来用", label)
	}
	if !strings.Contains(label, "标签") {
		t.Errorf("Label() = %q —— 没说清为什么归不出 workload", label)
	}
	if got := (reconcile.Subject{Namespace: "payment", Workload: "api"}).Label(); got != "payment/api" {
		t.Errorf("正常主体 Label() = %q, want payment/api", got)
	}
}
