package httpapi

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/store"
)

// renderedLines 把渲染出来的行收进一个切片，供断言。
func renderedLines(render func(line func(string, ...any))) []string {
	var out []string
	render(func(format string, args ...any) {
		out = append(out, fmt.Sprintf(format, args...))
	})
	return out
}

func joined(lines []string) string { return strings.Join(lines, "\n") }

// 没观测到流量时，四类计数一个都不许出现。**这是提交信息这一侧新修的那条**：
// 注释头本来就有这段判断，提交信息没有，于是同一次判定在文件里说"没人评估
// 过"、在合并请求上说"WOULD_BREAK: 0"，而后者是评审人唯一会读的那句话。
func TestBasisNeverPrintsZeroCountsWithoutObservation(t *testing.T) {
	pv := store.PolicyPreview{TrafficObserved: false}
	counts := map[predict.ChangeKind]int{}

	lines := renderedLines(func(line func(string, ...any)) { renderPolicyBasis(pv, counts, line) })
	text := joined(lines)

	for _, k := range predict.AllChangeKinds() {
		if strings.Contains(text, "dry-run "+string(k)+":") {
			t.Errorf("一条流量都没观测到，却印出了 dry-run %s 的计数 —— "+
				"那个 0 的意思是没评估过，不是没影响:\n%s", k, text)
		}
	}
	if !strings.Contains(text, "没有做过") {
		t.Errorf("没观测到流量时必须明说 dry-run 没做过，留空与老格式分不出来:\n%s", text)
	}
}

// 观测到了就照常印四类计数，一个不少。
func TestBasisPrintsEveryChangeKindWhenObserved(t *testing.T) {
	pv := store.PolicyPreview{TrafficObserved: true}
	counts := map[predict.ChangeKind]int{}

	text := joined(renderedLines(func(line func(string, ...any)) { renderPolicyBasis(pv, counts, line) }))
	for _, k := range predict.AllChangeKinds() {
		if !strings.Contains(text, "dry-run "+string(k)+":") {
			t.Errorf("少了 dry-run %s 那一行:\n%s", k, text)
		}
	}
}

// 一个缺口都没有时要**明说没有**。留空的那一份与老格式的、与渲染器坏掉的
// 长得一模一样，而三者含义完全不同。
func TestCaveatsSayNoneOutLoudInsteadOfGoingSilent(t *testing.T) {
	pv := store.PolicyPreview{WindowCompleteness: flow.CompletenessComplete}

	text := joined(renderedLines(func(line func(string, ...any)) { renderPolicyCaveats(pv, line) }))
	if !strings.Contains(text, "没有其他缺口") {
		t.Errorf("没有缺口时要显式说出来:\n%s", text)
	}
}

// 完整度永远打印，认不出的取值也要照原样印，不许静默。一个认不出的完整度
// 被当成"没问题"是这里最坏的失败方式。
func TestCaveatsNeverSwallowAnUnrecognisedCompleteness(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{string(flow.CompletenessComplete), "COMPLETE"},
		{string(flow.CompletenessDegraded), "DEGRADED"},
		{string(flow.CompletenessUnknown), "UNKNOWN"},
		{"", `""`},
		{"SOMETHING_NEW", `"SOMETHING_NEW"`},
	} {
		pv := store.PolicyPreview{WindowCompleteness: flow.Completeness(tc.in)}
		text := joined(renderedLines(func(line func(string, ...any)) { renderPolicyCaveats(pv, line) }))
		if !strings.Contains(text, "观测窗口完整度: ") {
			t.Fatalf("完整度那一行没了（取值 %q）:\n%s", tc.in, text)
		}
		if !strings.Contains(text, tc.want) {
			t.Errorf("完整度 %q 没有出现在文字里（要找 %s）:\n%s", tc.in, tc.want, text)
		}
	}
}

// 逐条列举超出上限时要说清楚还有多少条。静默截断读起来就是"只有这些"，
// 而那正是这几行要防的那句话。
func TestCaveatListSaysHowManyItLeftOut(t *testing.T) {
	pv := store.PolicyPreview{}
	for i := 0; i < caveatListCap+5; i++ {
		pv.ExcludedNamespaces = append(pv.ExcludedNamespaces, policygen.ExcludedNamespace{
			Namespace: "ns" + fmt.Sprintf("%d", i),
			Reason:    policygen.NamespaceExclusionSystem,
		})
	}

	text := joined(renderedLines(func(line func(string, ...any)) { renderPolicyCaveats(pv, line) }))
	if !strings.Contains(text, "排除的命名空间: 13 个") {
		t.Errorf("总数要说全，不能只说列出来的那几条:\n%s", text)
	}
	if !strings.Contains(text, "另有 5 条未列出") {
		t.Errorf("截断了就要说还剩多少条:\n%s", text)
	}
}

// 生成不出规则的流按封闭枚举归并，**自由文本的 Detail 一个字都不许出现**。
// 这段文字会落进仓库历史，而 Detail 会把命名空间/Pod 名与标签取值插进去。
func TestUngeneratableNeverCarriesFreeText(t *testing.T) {
	// 一段像集群内容的自由文本。名字不带 secret/token 字样：这里要防的是
	// 集群对象名泄进仓库历史，不是凭据。
	const freeText = "payment-7f9c/leaked-detail"
	pv := store.PolicyPreview{Ungeneratable: []policygen.UngeneratableItem{
		{FlowID: "f1", Reason: policygen.ReasonIdentityUnknown, Detail: freeText},
		{FlowID: "f2", Reason: policygen.ReasonIdentityUnknown, Detail: freeText},
	}}

	text := joined(renderedLines(func(line func(string, ...any)) { renderPolicyCaveats(pv, line) }))
	if strings.Contains(text, freeText) {
		t.Errorf("Detail 是自由文本，不许进这段文字:\n%s", text)
	}
	if !strings.Contains(text, string(policygen.ReasonIdentityUnknown)+"：2 条") {
		t.Errorf("要按封闭枚举归并并给出条数:\n%s", text)
	}
}

// ExposureWidenings 是每条暴露规则一行的全账，不是放宽清单。ExtraPods 为 0
// 的那些不是放宽——把它们也报出来，是在一份会被应用到生产集群的文件里断言
// 一件没发生的事。
func TestExposureWideningReportsOnlyActualWidening(t *testing.T) {
	pv := store.PolicyPreview{ExposureWidenings: []policygen.ExposureWidening{
		{Namespace: "shop", Service: "api-lb", Workload: "api",
			SelectedPods: 3, WorkloadPods: 3, ExtraPods: 0},
	}}
	text := joined(renderedLines(func(line func(string, ...any)) { renderPolicyCaveats(pv, line) }))
	if strings.Contains(text, "暴露放宽") {
		t.Errorf("ExtraPods=0 不是放宽，不该出现在缺口里:\n%s", text)
	}
	if !strings.Contains(text, "没有其他缺口") {
		t.Errorf("唯一那条不是放宽，整体就该是没有缺口:\n%s", text)
	}

	pv.ExposureWidenings = append(pv.ExposureWidenings, policygen.ExposureWidening{
		Namespace: "shop", Service: "admin-lb", Workload: "admin",
		SelectedPods: 1, WorkloadPods: 4, ExtraPods: 3,
	})
	text = joined(renderedLines(func(line func(string, ...any)) { renderPolicyCaveats(pv, line) }))
	if !strings.Contains(text, "暴露放宽: 1 个 Service") {
		t.Errorf("只有一条是真放宽，计数要是 1 而不是 2:\n%s", text)
	}
	if !strings.Contains(text, "规则覆盖 4 个 Pod，其中 3 个 Service 并没有暴露") {
		t.Errorf("放宽的方向说反了或数错了:\n%s", text)
	}
	if strings.Contains(text, "api-lb") {
		t.Errorf("没放宽的那条不该被列出来:\n%s", text)
	}
}

// 证据停摆时，注释头第一句就要说出来。
//
// 2026-08-29 事故的教训：记账连续失败 13 小时，而界面上唯一的症状是几个
// 数字不再变大——"不再变大"与"这段时间确实没有新观测"长得一模一样。
// 一份基于停摆证据的策略文件，读者手上只有这段文字。
func TestStaleEvidenceIsSaidFirst(t *testing.T) {
	pv := store.PolicyPreview{
		WindowCompleteness: flow.CompletenessComplete,
		EvidenceLag: store.EvidenceLag{
			AccountedTo: mustTime("2026-08-29T00:06:56Z"),
			IngestedTo:  mustTime("2026-08-29T13:21:00Z"),
		},
	}
	lines := renderedLines(func(line func(string, ...any)) { renderPolicyCaveats(pv, line) })
	text := joined(lines)

	if !strings.Contains(text, "证据已停止更新") {
		t.Fatalf("证据停了 13 小时，注释头一个字都没说:\n%s", text)
	}
	// 排在完整度前面：完整度也是记账那一刻算出来的，先说它会让读者
	// 把一个过期的 COMPLETE 当成此刻的事实。
	stale, complete := -1, -1
	for i, l := range lines {
		if stale < 0 && strings.Contains(l, "证据已停止更新") {
			stale = i
		}
		if complete < 0 && strings.Contains(l, "观测窗口完整度") {
			complete = i
		}
	}
	if stale < 0 || complete < 0 || stale > complete {
		t.Errorf("停摆那一行排在完整度后面（stale=%d complete=%d）:\n%s", stale, complete, text)
	}
	if !strings.Contains(text, "13h") && !strings.Contains(text, "落后") {
		t.Errorf("没说落后了多久:\n%s", text)
	}
}

// 没停摆时不许出现这段：一条常年挂着的警告等于没有警告。
func TestFreshEvidenceSaysNothingAboutStaleness(t *testing.T) {
	pv := store.PolicyPreview{
		WindowCompleteness: flow.CompletenessComplete,
		EvidenceLag: store.EvidenceLag{
			AccountedTo: mustTime("2026-08-29T13:57:00Z"),
			IngestedTo:  mustTime("2026-08-29T13:58:00Z"),
		},
	}
	text := joined(renderedLines(func(line func(string, ...any)) { renderPolicyCaveats(pv, line) }))
	if strings.Contains(text, "证据已停止更新") {
		t.Errorf("证据是新的却报了停摆 —— 常年挂着的警告等于没有警告:\n%s", text)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
