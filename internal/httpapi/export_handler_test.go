package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

const exportPath = "/api/v1/clusters/prod-asia-1/policy-export"

// previewView 是预览响应里本组用例要读的部分。
//
// 只声明这几个字段，而不是复用 store.PolicyPreview：这些用例断言的是
// **线上真的传给了前端的那份 JSON**，与导出文件对不对得上。拿被测类型
// 自己去解自己的输出，一个字段被改成不序列化也照样绿。
type previewView struct {
	Cluster   string           `json:"cluster"`
	Namespace string           `json:"namespace"`
	Window    store.TimeWindow `json:"window"`
	// Candidates 与 Prediction 是**默认（覆盖前）**那一套，导出永远不该
	// 用它们。读进来是为了先决地证明两套计算确实不同 —— 两者相等的状态
	// 下，任何"导出用了哪一套"的断言都无法失败。
	Candidates []policygen.CandidatePolicy `json:"candidates"`
	Prediction predict.Report              `json:"prediction"`
	Overrides  []registry.RuleOverride     `json:"overrides"`
	Overridden struct {
		Candidates []policygen.CandidatePolicy `json:"candidates"`
		Prediction predict.Report              `json:"prediction"`
	} `json:"overridden"`
}

// fetchPreview 取一次预览响应，即操作者屏幕上那一份。
func fetchPreview(t *testing.T, h http.Handler, cookie *http.Cookie, query string) previewView {
	t.Helper()
	rec := authedGet(t, h, cookie, previewPath+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int         `json:"code"`
		Data previewView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if env.Code != 0 {
		t.Fatalf("preview code = %d, want 0 (%s)", env.Code, rec.Body.String())
	}
	return env.Data
}

// exportDocs 把导出响应拆成注释头与 NetworkPolicy 文档。
func exportDocs(t *testing.T, rec *httptest.ResponseRecorder) (string, []networkingv1.NetworkPolicy) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	parts := strings.Split(rec.Body.String(), "\n---\n")
	if len(parts) < 2 {
		t.Fatalf("export body has no document separator:\n%s", rec.Body.String())
	}
	docs := make([]networkingv1.NetworkPolicy, 0, len(parts)-1)
	for _, raw := range parts[1:] {
		var np networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(raw), &np); err != nil {
			t.Fatalf("document is not a NetworkPolicy: %v\n%s", err, raw)
		}
		docs = append(docs, np)
	}
	return parts[0], docs
}

// 导出与预测必须来自同一次计算（design doc 2026-08-14 §2）。
//
// **这条用例比的是规则内容，不是条数。** 三个方向同时钉住：
//
//  1. 文件里的每一份文档与 Reader 在**同一组入参**下算出来的那一份
//     逐字段相等（podSelector、每条 ingress/egress 的对端与端口）——
//     导出若换一个窗口、或自己再生成一次，内容会变，这里就红；
//  2. 文档集与**预览响应真的传给前端的那份候选集**逐条对应：workload
//     名册相同，每份文档每个方向的规则条数等于该 workload 下启用规则
//     的条数 —— 把文件绑到屏幕上那一份，而不是绑到某次内部计算；
//  3. 注释头里的四类计数等于预览响应里那一套 overridden 计数。
//
// 两个窗口各跑一遍，且默认窗口被显式设成与自定义窗口不同的那一个：
// 一个"忽略入参、按默认窗口重新生成"的实现在默认窗口那一轮是看不出来的。
func TestPolicyExportRendersTheSameRuleSetThePreviewReported(t *testing.T) {
	reader := fixtureReader()
	full := reader.(*store.FixtureReader).DataWindow()
	// 自定义窗口取开头 60 秒：fixture 里任何自定义窗口的候选集都是全量窗口
	// 候选集的子集，而这一段窄到**规则内容本身**就不一样（不只是预测数字
	// 不一样）—— 否则第 1 条方向（逐字段相等）在这一轮验证不了任何东西。
	narrow := store.TimeWindow{From: full.From, To: full.From.Add(60 * time.Second)}
	if !narrow.To.Before(full.To) {
		t.Fatal("fixture data window too short for this test's narrow slice")
	}

	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithWindow(t, reader, reg, full)

	cases := []struct{ name, namespace, query string }{
		{"默认窗口", "", ""},
		{"自定义窗口", "", fmt.Sprintf("?from=%s&to=%s",
			narrow.From.UTC().Format(time.RFC3339), narrow.To.UTC().Format(time.RFC3339))},
	}

	bodies := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pv := fetchPreview(t, h, cookie, c.query)
			rec := authedGet(t, h, cookie, exportPath+c.query)
			header, docs := exportDocs(t, rec)
			bodies[c.name] = rec.Body.String()

			// 方向 1：与 Reader 在同一组入参下的产物逐字段相等。
			want, err := reader.PolicyPreviewAtGranularity(context.Background(), "prod-asia-1",
				c.namespace, pv.Window, policygen.GranularityWorkload)
			if err != nil {
				t.Fatalf("PolicyPreview() error = %v", err)
			}
			if len(docs) != len(want.Overridden.Enabled) {
				t.Fatalf("exported %d documents, the same computation yields %d",
					len(docs), len(want.Overridden.Enabled))
			}
			for i, got := range docs {
				if got.Kind != "NetworkPolicy" ||
					got.APIVersion != "networking.k8s.io/v1" {
					t.Errorf("document %d TypeMeta = %+v — the file must be applyable as it stands",
						i, got.TypeMeta)
				}
				got.TypeMeta = metav1.TypeMeta{}
				if !reflect.DeepEqual(got, want.Overridden.Enabled[i]) {
					t.Fatalf("document %d differs from the rule set this window computes:\ngot  %+v\nwant %+v",
						i, got, want.Overridden.Enabled[i])
				}
			}

			// 方向 2：与预览响应里那份候选集对应。
			assertDocsMatchCandidates(t, docs, pv.Overridden.Candidates)

			// 方向 3：注释头里的数字就是屏幕上那几个。
			for _, k := range predict.AllChangeKinds() {
				want := fmt.Sprintf("# dry-run %s: %d", k, pv.Overridden.Prediction.Counts[k])
				if !strings.Contains(header, want) {
					t.Errorf("header missing %q:\n%s", want, header)
				}
			}
		})
	}

	// 两轮必须真的产出不同的文件，否则上面那轮参数化是摆设：一个
	// "入参照收、一律按默认窗口生成"的实现会在两轮里都通过。
	if bodies["默认窗口"] == bodies["自定义窗口"] {
		t.Fatal("both windows exported byte-identical files — this test cannot tell the two computations apart")
	}
}

// assertDocsMatchCandidates 断言文档集与预览响应里的候选集逐条对应。
//
// 比的是"哪个 workload、每个方向几条规则、podSelector 是什么"，不是一个
// 总数：一个总数相同、但把 A 的规则搬到 B 名下的文件，在总数断言下是绿的。
func assertDocsMatchCandidates(
	t *testing.T, docs []networkingv1.NetworkPolicy, candidates []policygen.CandidatePolicy,
) {
	t.Helper()

	type shape struct {
		namespace, name string
		selector        string
		ingress, egress int
	}
	fromDocs := make([]shape, 0, len(docs))
	for _, d := range docs {
		fromDocs = append(fromDocs, shape{
			namespace: d.Namespace, name: d.Name,
			selector: fmt.Sprint(d.Spec.PodSelector.MatchLabels),
			ingress:  len(d.Spec.Ingress), egress: len(d.Spec.Egress),
		})
	}

	fromPreview := make([]shape, 0, len(candidates))
	for _, c := range candidates {
		s := shape{
			namespace: c.Namespace, name: "candidate-" + c.Workload,
			selector: fmt.Sprint(map[string]string{c.WorkloadLabelKey: c.Workload}),
		}
		for _, r := range c.Rules {
			// 被否决的规则不出现在文件里，也不以注释形式出现（§4）。
			if !r.Enabled {
				continue
			}
			switch r.Direction {
			case "INGRESS":
				s.ingress++
			case "EGRESS":
				s.egress++
			default:
				t.Fatalf("candidate %s/%s has a rule with an unexpected direction %q",
					c.Namespace, c.Workload, r.Direction)
			}
		}
		fromPreview = append(fromPreview, s)
	}

	sortShapes := func(in []shape) {
		sort.Slice(in, func(i, j int) bool {
			if in[i].namespace != in[j].namespace {
				return in[i].namespace < in[j].namespace
			}
			return in[i].name < in[j].name
		})
	}
	sortShapes(fromDocs)
	sortShapes(fromPreview)
	if !reflect.DeepEqual(fromDocs, fromPreview) {
		t.Errorf("the file does not correspond to the candidate set the preview returned:\nfile    %+v\npreview %+v",
			fromDocs, fromPreview)
	}
}

// 文件与注释头必须**同时**来自操作者读过的那次"应用人工决定之后"的计算
// （design doc 2026-08-14 §2）。
//
// 有两个独立的选择点：文件渲染自哪一套候选集（store 里 Overridden.Enabled
// 的来源），以及注释头取哪一套四类计数。任一处换成默认（覆盖前）那一套，
// 操作者拿到的文件就与他确认过的决定分了家，而屏幕上没有任何迹象 ——
// 轮 3 出过同一形状的 Critical，四道门禁全绿。
//
// 这条用例是本包里唯一一个**看得见人工决定**的：其余用例走 fixtureReader()，
// 它的覆盖来源与 handler 写入的注册表是两个不同的 memRegistry，两套计算于是
// 恒等，把任一选择点换成覆盖前都测不出来。这里让 Reader 与 handler 共用
// 同一个注册表，先决地钉住两套计算确实不同，断言才可能红。
//
// 数字与内容两头都断言：同一个窗口下换一套规则集，四类计数未必跟着变，
// 只断其中一头会因为错误的理由通过。
func TestPolicyExportCarriesTheConfirmedOverrideIntoBothFileAndHeader(t *testing.T) {
	reg := fixtureSource()
	reader := store.NewFixtureReader(fixture.Load(), reg)
	h, _, cookie := newTestRouterWithRegistry(t, reader, reg)

	ns, wl, fp := singleSidedDisabledRule(t, fetchPreview(t, h, cookie, ""))

	create := authedPostJSON(t, h, cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "导出必须带上这条确认"})
	if got := bodyOf(t, create)["code"]; got != float64(0) {
		t.Fatalf("create override code = %v, want 0 (%s)", got, create.Body.String())
	}

	// 操作者屏幕上的那一份 —— 确认之后重新读到的报告。
	pv := fetchPreview(t, h, cookie, "")
	if len(pv.Overrides) == 0 {
		t.Fatal("the preview reports no override — the reader and the registry are not the same source")
	}

	// 先决条件一：两套计数确实不同，否则注释头那条断言无法失败。
	if reflect.DeepEqual(pv.Prediction.Counts, pv.Overridden.Prediction.Counts) {
		t.Fatalf("both computations report the same counts %v — this test cannot tell them apart",
			pv.Prediction.Counts)
	}
	// 先决条件二：目标 workload 的启用规则集确实不同，否则文件那条断言
	// 无法失败 —— 计数变了而内容没变的状态下，只有数字能被区分。
	before := enabledRuleCount(pv.Candidates, ns, wl)
	after := enabledRuleCount(pv.Overridden.Candidates, ns, wl)
	if after != before+1 {
		t.Fatalf("%s/%s enabled rules: default = %d, overridden = %d, want overridden = default+1",
			ns, wl, before, after)
	}

	header, docs := exportDocs(t, authedGet(t, h, cookie, exportPath))

	// 断言一：文件里那份文档带着这条确认，不是覆盖前的规则集。
	if got := docRuleCount(t, docs, ns, wl); got != after {
		t.Errorf("exported document %s/candidate-%s has %d rules, the report the operator read says %d "+
			"(%d before the override) — the file was rendered from a computation he never saw",
			ns, wl, got, after, before)
	}
	// 断言二：整份文件逐个 workload 对应覆盖后的候选集，不只是那一条。
	assertDocsMatchCandidates(t, docs, pv.Overridden.Candidates)
	// 断言三：注释头里的四类计数是覆盖后的那一套。
	for _, k := range predict.AllChangeKinds() {
		want := fmt.Sprintf("# dry-run %s: %d", k, pv.Overridden.Prediction.Counts[k])
		if !strings.Contains(header, want) {
			t.Errorf("header missing %q — 头里的数字来自操作者没读过的那次计算:\n%s", want, header)
		}
	}
}

// singleSidedDisabledRule 取一条默认禁用、且对端在本集群候选策略集之外的
// LEARNED 规则。
//
// 限定 INTERNET_EGRESS / CROSS_CLUSTER：集群内的一条连接在两侧各生成一条
// 独立规则（源端 egress、目的端 ingress），NetworkPolicy 要两侧都放行才算
// 放行，只启用其中一条不会翻转任何判定 —— 覆盖生效了，四类计数却纹丝不动，
// 上面那条先决条件就会把用例判死。这两类的对端是公网 IP 或另一个集群，
// 启用一条就足以让判定改变。
func singleSidedDisabledRule(t *testing.T, pv previewView) (string, string, string) {
	t.Helper()
	for _, p := range pv.Candidates {
		for _, r := range p.Rules {
			outside := r.Evidence == policygen.EvidenceInternetEgress ||
				r.Evidence == policygen.EvidenceCrossCluster
			if r.Origin == policygen.OriginLearned && !r.Enabled && outside {
				return p.Namespace, p.Workload, r.Fingerprint
			}
		}
	}
	t.Fatal("fixture produced no disabled learned rule whose peer is outside this cluster's candidate set")
	return "", "", ""
}

// enabledRuleCount 数候选集里某个 workload 下的启用规则条数。
func enabledRuleCount(candidates []policygen.CandidatePolicy, ns, wl string) int {
	n := 0
	for _, c := range candidates {
		if c.Namespace != ns || c.Workload != wl {
			continue
		}
		for _, r := range c.Rules {
			if r.Enabled {
				n++
			}
		}
	}
	return n
}

// docRuleCount 数导出文件里某个 workload 那份文档的规则条数。找不到即失败：
// 一份少了整份文档的文件，与一份少了一条规则的文件是同一类问题。
func docRuleCount(t *testing.T, docs []networkingv1.NetworkPolicy, ns, wl string) int {
	t.Helper()
	for _, d := range docs {
		if d.Namespace == ns && d.Name == "candidate-"+wl {
			return len(d.Spec.Ingress) + len(d.Spec.Egress)
		}
	}
	t.Fatalf("exported file has no document for %s/candidate-%s", ns, wl)
	return 0
}

// 注释头必须自述，且**只**有这几行：文件会脱离平台独自存在，那段话是
// 事后唯一能回答"当初凭什么认为它安全"的东西（§3）。同时它不得带出任何
// 凭据、host key、内部地址或账号哈希 —— 因此这里钉的是一份封闭清单，
// 多出来的一行会红，而不是"看看有没有出现某个敏感词"。
func TestPolicyExportHeaderIsSelfDescribingAndNothingMore(t *testing.T) {
	reader := fixtureReader()
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, reader, reg)

	pv := fetchPreview(t, h, cookie, "")
	rec := authedGet(t, h, cookie, exportPath)
	header, docs := exportDocs(t, rec)

	allowed := []string{
		"# Distill 已确认策略导出",
		"# 集群: ",
		"# 命名空间筛选: ",
		"# 时间窗: ",
		"# dry-run WOULD_BREAK: ", "# dry-run WOULD_OPEN: ",
		"# dry-run UNCHANGED: ", "# dry-run UNKNOWN: ",
		"# 策略文档: ",
		"# 导出者: ",
		"# 导出时刻: ",
		"# 以上 dry-run 结论算的是导出这一刻的集群状态",
		"# 集群在此之后发生的变化不在其中",
	}
	for _, line := range strings.Split(strings.TrimSuffix(header, "\n"), "\n") {
		known := false
		for _, prefix := range allowed {
			if strings.HasPrefix(line, prefix) {
				known = true
				break
			}
		}
		if !known {
			t.Errorf("header line %q is not on the list this test allows —— "+
				"新增一行要先说明它不含凭据、host key、内部地址或哈希", line)
		}
	}

	for _, want := range []string{
		"# 集群: prod-asia-1",
		"# 命名空间筛选: 全集群",
		"# 时间窗: " + pv.Window.From.UTC().Format(time.RFC3339) +
			" ~ " + pv.Window.To.UTC().Format(time.RFC3339),
		fmt.Sprintf("# 策略文档: %d 份", len(docs)),
		"# 导出者: demo",
	} {
		if !strings.Contains(header, want) {
			t.Errorf("header missing %q:\n%s", want, header)
		}
	}
	// 「这些数字是导出那一刻的结论，不是应用时刻的」必须白纸黑字写着。
	if !strings.Contains(header, "不是应用这一刻的") {
		t.Errorf("header does not say the numbers describe the moment of export:\n%s", header)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/yaml") {
		t.Errorf("Content-Type = %q, want text/yaml", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment; filename=") || !strings.Contains(cd, "prod-asia-1") {
		t.Errorf("Content-Disposition = %q, want an attachment named after the cluster", cd)
	}
}

// noEnabledRulesReader 让预览算得出结果、但一条启用规则都不剩。
//
// 包一层真实 Reader 而不是整个替换掉：除这一处以外的行为（集群/命名空间
// 判定、窗口回显）仍然是真的，被测的只有"没有可导出内容时怎么办"。
type noEnabledRulesReader struct{ store.Reader }

func (r noEnabledRulesReader) PolicyPreviewAtGranularity(
	ctx context.Context, clusterID, namespace string, window store.TimeWindow,
	g policygen.Granularity,
) (store.PolicyPreview, error) {
	pv, err := r.Reader.PolicyPreviewAtGranularity(ctx, clusterID, namespace, window, g)
	if err != nil {
		return pv, err
	}
	pv.Overridden.Candidates = nil
	pv.Overridden.Enabled = nil
	return pv, nil
}

// 空结果拒绝导出，而不是产出一个空文件（§4）：一份空的 policy 文件
// 应用下去是"什么都不允许"还是"什么都没做"，取决于集群里已有什么。
func TestPolicyExportRefusesWhenNothingIsEnabled(t *testing.T) {
	reader := fixtureReader()
	full := reader.(*store.FixtureReader).DataWindow()
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithWindow(t,
		noEnabledRulesReader{Reader: reader}, reg, full)

	rec := authedGet(t, h, cookie, exportPath)
	body := bodyOf(t, rec)
	if got := body["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 — an empty result must be refused (%s)", got, rec.Body.String())
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "没有任何启用中的规则") {
		t.Errorf("msg = %q, want a stated reason for the refusal", msg)
	}
	if strings.Contains(rec.Body.String(), "NetworkPolicy") {
		t.Errorf("a refused export still produced policy content: %s", rec.Body.String())
	}
	// 什么都没导出，就没有什么可审计的 —— 但更重要的是反过来：
	// 一条"导出过"的审计行若在这里出现，事后追溯会指向一份从不存在的文件。
	if len(reg.policyExports) != 0 {
		t.Errorf("a refused export wrote %d audit rows, want 0", len(reg.policyExports))
	}
}

// 导出写审计（§5，规范 §43「导出数据」）：谁在什么时候把这个集群的
// 策略拿走了，必须答得出来。断言的是那条记录的内容，不是它的条数。
func TestPolicyExportWritesAnAuditRow(t *testing.T) {
	reader := fixtureReader()
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, reader, reg)

	pv := fetchPreview(t, h, cookie, "")
	rec := authedGet(t, h, cookie, exportPath)
	_, docs := exportDocs(t, rec)

	if len(reg.policyExports) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(reg.policyExports))
	}
	got := reg.policyExports[0]

	rules := 0
	for _, d := range docs {
		rules += len(d.Spec.Ingress) + len(d.Spec.Egress)
	}
	want := registry.PolicyExport{
		ClusterID: "prod-asia-1", Namespace: "",
		From: pv.Window.From, To: pv.Window.To,
		Documents: len(docs), Rules: rules,
	}
	if !got.From.Equal(want.From) || !got.To.Equal(want.To) {
		t.Errorf("audited window = %s ~ %s, want %s ~ %s —— 换一个窗口导出的是另一套规则",
			got.From, got.To, want.From, want.To)
	}
	got.From, got.To = want.From, want.To
	if !reflect.DeepEqual(got, want) {
		t.Errorf("audit row = %+v, want %+v", got, want)
	}
}

// 审计写不进去，这次导出就不发生：一次没有留下痕迹的导出，事后没有任何
// 东西能回答"谁把它拿走了"。失败的方向必须是不给文件，不是不写审计。
func TestPolicyExportFailsWhenTheAuditCannotBeWritten(t *testing.T) {
	reader := fixtureReader()
	reg := newRegisteredRegistry()
	reg.failWritesWith = fmt.Errorf("audit sink down at 10.0.0.5:3306")
	h, _, cookie := newTestRouterWithRegistry(t, reader, reg)

	rec := authedGet(t, h, cookie, exportPath)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "NetworkPolicy") {
		t.Errorf("the policy document was served despite the audit failing: %s", rec.Body.String())
	}
	// 依赖故障的真实原因只进日志（规范 §22）。
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("the response leaked an internal address: %s", rec.Body.String())
	}
}

// 与 policy-preview 用同一个 parseWindow：半成品窗口在两个端点上必须
// 是同一个答案，否则两者会因为解析差异给出不同的规则集（§5）。
func TestPolicyExportRejectsHalfWindowLikePreview(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newRegisteredRegistry())

	const half = "?from=2026-07-31T10:00:00Z"
	export := authedGet(t, h, cookie, exportPath+half)
	preview := authedGet(t, h, cookie, previewPath+half)

	if export.Code != preview.Code || bodyOf(t, export)["code"] != bodyOf(t, preview)["code"] {
		t.Errorf("export = HTTP %d/%v, preview = HTTP %d/%v; the same query must answer the same",
			export.Code, bodyOf(t, export)["code"], preview.Code, bodyOf(t, preview)["code"])
	}
}

func TestPolicyExportRequiresSession(t *testing.T) {
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), newRegisteredRegistry())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, exportPath, nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// 未知集群与未知命名空间是业务失败，且不得留下审计行。
func TestPolicyExportUnknownTargetIsBusinessFailure(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	const path = "/api/v1/clusters/nope/policy-export"
	rec := authedGet(t, h, cookie, path)
	if rec.Code != http.StatusOK {
		t.Errorf("%s status = %d, want 200 —— 目标不存在是业务失败", path, rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("%s code = %v, want 20002", path, got)
	}
	if len(reg.policyExports) != 0 {
		t.Errorf("a failed export wrote %d audit rows, want 0", len(reg.policyExports))
	}
}

// 按命名空间筛选的导出必须拒绝（design doc 2026-08-14 §4）。
//
// 预测恒按整集群计算，一份被裁剪的文件配上整集群口径的注释头，描述的是
// 另一套策略集：头里的数字把文件里根本不存在的规则也算了进去，且朝着让人
// 放心的方向偏。这与"空文件"是同一类歧义，只是戴着一个更有说服力的头。
//
// 同一条用例里紧跟着一次不带筛选的导出，是必要的：一个"一律拒绝"的实现
// 会让上半段永远通过，而那等于把这个功能整个关掉。
func TestPolicyExportRefusesANamespaceFilter(t *testing.T) {
	reg := newRegisteredRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	// 存在的命名空间同样拒绝：拒的是"筛选"这件事，不是"这个名字不认识"。
	filtered := authedGet(t, h, cookie, exportPath+"?namespace=payment")
	body := bodyOf(t, filtered)
	if got := body["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 — a filtered export must be refused (%s)",
			got, filtered.Body.String())
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "按整集群计算") {
		t.Errorf("msg = %q, want the reason the filtered file cannot carry an honest header", msg)
	}
	if strings.Contains(filtered.Body.String(), "NetworkPolicy") {
		t.Errorf("a refused export still produced policy content: %s", filtered.Body.String())
	}
	if len(reg.policyExports) != 0 {
		t.Errorf("a refused export wrote %d audit rows, want 0", len(reg.policyExports))
	}

	// 不带筛选仍然导得出来 —— 这条守的是"守卫没有把功能一起关掉"。
	whole := authedGet(t, h, cookie, exportPath)
	_, docs := exportDocs(t, whole)
	if len(docs) == 0 {
		t.Fatal("an unfiltered export produced no documents — the guard disabled the feature")
	}
	if len(reg.policyExports) != 1 {
		t.Errorf("audit rows after the unfiltered export = %d, want 1", len(reg.policyExports))
	}
}
