package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/baseline"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/gitwrite"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

const (
	euPlanPath   = "/api/v1/clusters/prod-eu-1/policy-writeback/plan"
	euPushPath   = "/api/v1/clusters/prod-eu-1/policy-writeback/push"
	euPolicyPath = "clusters/prod-eu-1"
)

// newEUWritebackFixture 装一个绑定了策略仓库的 prod-eu-1。
//
// 与 asia 那个替身的唯一区别是集群：eu 的 partner namespace 真的缺两类
// Baseline（有 LoadBalancer Service、有被抓声明，两者都推不出规则），
// 因此它是门禁那一侧的样本。asia 一条不缺，是放行那一侧。
func newEUWritebackFixture(t *testing.T) *writebackFixture {
	t.Helper()

	reg := fixtureSource()
	reg.repos[gitRepoID] = boundRepo()
	c := reg.clusters["prod-eu-1"]
	c.Git = &registry.GitBinding{
		RepoID: gitRepoID, PolicyPath: euPolicyPath,
		VerifyResult: registry.BindingVerifyNotVerified,
	}
	reg.clusters["prod-eu-1"] = c

	reader := store.NewFixtureReader(fixture.Load(), reg)
	gv := &stubGitVerifier{result: registry.BindingVerifyOK}
	writer := &fakePolicyWriter{listing: gitwrite.RepoListing{}}
	var logs bytes.Buffer
	h, _, cookie := buildTestRouterWithLog(t, reader, reg, gv, writer, nil, "ERROR", &logs)

	return &writebackFixture{
		h: h, cookie: cookie, reg: reg, reader: reader,
		verifier: gv, writer: writer, logs: &logs,
	}
}

// notAssessedReader 让 DNS 那一类报成"这次采集没拿回依据"。
//
// 包一层而不是改 fixture：未评估讲的是那一次采集拿回了什么，与合成数据集
// 无关 —— 而门禁必须在这种形态下也挡住。
type notAssessedReader struct {
	store.Reader
}

func (r notAssessedReader) PolicyPreview(
	ctx context.Context, clusterID, namespace string, w store.TimeWindow,
) (store.PolicyPreview, error) {
	pv, err := r.Reader.PolicyPreview(ctx, clusterID, namespace, w)
	if err != nil {
		return pv, err
	}
	pv.NotAssessedBaselines = []baseline.Kind{baseline.KindDNS}
	return pv, nil
}

// refusal 读出一次业务拒绝的码与文案。
func refusal(t *testing.T, body string) (int, string) {
	t.Helper()
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return env.Code, env.Msg
}

// Baseline 不齐备的集群不许出计划 —— 推出去的每条策略都固定
// policyTypes: Ingress+Egress，规则为空即 default-deny，因此推送那一刻
// 就是进入 Enforcing（design doc 2026-08-18-enforcing-gate §1）。
func TestPlanIsRefusedWhenAPushedNamespaceIsMissingABaseline(t *testing.T) {
	f := newEUWritebackFixture(t)

	rec := authedPostJSON(t, f.h, f.cookie, euPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("plan succeeded for a cluster whose partner namespace is missing two baselines: %s", rec.Body.String())
	}
	if !strings.Contains(msg, "partner") {
		t.Errorf("refusal does not name the namespace that blocks it: %q", msg)
	}
	if !strings.Contains(msg, "LB_HEALTH_CHECK") || !strings.Contains(msg, "METRICS_SCRAPE") {
		t.Errorf("refusal does not name the missing kinds: %q", msg)
	}
	if f.writer.calls != 0 {
		t.Error("a refused plan reached the repository")
	}
}

// 同一道门必须挡住推送。计划都出不来的集群，推送更不该有路径进去。
func TestPushIsRefusedWhenAPushedNamespaceIsMissingABaseline(t *testing.T) {
	f := newEUWritebackFixture(t)

	rec := authedPostJSON(t, f.h, f.cookie, euPushPath, map[string]any{
		"branch":      "distill/prod-eu-1-20260818T090000Z",
		"fingerprint": "whatever",
	})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("push succeeded despite a missing baseline: %s", rec.Body.String())
	}
	// 必须是**门禁**拒的，不是指纹对不上拒的：后者会让这条用例在门禁被
	// 整个删掉之后照样绿。门禁排在指纹比对之前，因此文案里必须有那个
	// namespace。
	if !strings.Contains(msg, "partner") {
		t.Errorf("push was refused, but not by the baseline gate: %q", msg)
	}
	if f.writer.calls != 0 {
		t.Error("a refused push still wrote to the repository")
	}
}

// **本轮不得让原本能推的推不了。** asia 五类齐备（其余 namespace 是不适用，
// 不是缺失），它必须照旧出得来计划、推得出去。
//
// 少了这条，一个"一律拒绝"的门禁实现会让上面两条永远通过，而那等于把写回
// 整个关掉。
func TestACompleteClusterStillPlansAndPushes(t *testing.T) {
	f := newWritebackFixture(t)

	plan := fetchPlan(t, f)
	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath, map[string]any{
		"branch": plan.Plan.Branch, "fingerprint": plan.Plan.Fingerprint,
	})
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != 0 {
		t.Fatalf("a cluster with no missing baseline was refused: %s", rec.Body.String())
	}
	if f.writer.calls != 1 {
		t.Errorf("pushes = %d, want 1", f.writer.calls)
	}
}

// 未评估与缺失同处置：没做过的检查不是通过了的检查（design doc §3）。
//
// 这一条守的是门禁只读 MissingBaselines 的那种写法 —— 依据采集一旦 403 或
// 超时，DNS 这种要紧的类会间歇性地离开缺失清单，于是一个从没验证过 DNS
// 依据的集群被放行进 Enforcing。
func TestPlanIsRefusedWhenABaselineWasNeverAssessed(t *testing.T) {
	f := newWritebackFixture(t)
	f.reader = notAssessedReader{Reader: f.reader}
	h, _, cookie := buildTestRouterWithLog(t, f.reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs)

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("plan succeeded while DNS evidence was never assessed: %s", rec.Body.String())
	}
	if !strings.Contains(msg, "DNS") {
		t.Errorf("refusal does not name the unassessed kind: %q", msg)
	}
}

// injectMissingReader 往缺失清单里塞一个**不会被写进文件**的 namespace。
type injectMissingReader struct {
	store.Reader
	namespace string
}

func (r injectMissingReader) PolicyPreview(
	ctx context.Context, clusterID, namespace string, w store.TimeWindow,
) (store.PolicyPreview, error) {
	pv, err := r.Reader.PolicyPreview(ctx, clusterID, namespace, w)
	if err != nil {
		return pv, err
	}
	pv.MissingBaselines = append(pv.MissingBaselines, policygen.MissingBaseline{
		Namespace: r.namespace, Kinds: []baseline.Kind{baseline.KindDNS},
	})
	return pv, nil
}

// 判的是这次真的会被写进文件的那些 namespace，不是整个集群。
//
// 没有策略落进去的 namespace 不会获得 default-deny，也就不会被打断。把它
// 算进来等于让一个与本次推送无关的缺口永久挡住所有推送，而一道永远在挡的
// 门会被整体绕开（design doc §2）。
func TestAMissingBaselineOutsideTheFileDoesNotBlockThePush(t *testing.T) {
	f := newWritebackFixture(t)
	// quarantine 不在候选策略里 —— 这份文件不会给它下发任何东西。
	reader := injectMissingReader{Reader: f.reader, namespace: "quarantine"}
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs)

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code != 0 {
		t.Fatalf("plan refused over a namespace it does not write to: %s", msg)
	}
}

// 一条规则都不推时，没有任何 default-deny 落地 —— 该说的是"没有可写内容"，
// 不是"前置未齐备"。两句话指向的处置完全不同。
func TestAnEmptySelectionReportsNothingToWriteNotAGateFailure(t *testing.T) {
	f := newEUWritebackFixture(t)
	reader := emptyEnabledReader{Reader: f.reader}
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs)

	rec := authedPostJSON(t, h, cookie, euPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("plan succeeded with nothing enabled: %s", rec.Body.String())
	}
	if strings.Contains(msg, "Baseline") || strings.Contains(msg, "DNS") {
		t.Errorf("refused as a gate failure when there was simply nothing to write: %q", msg)
	}
}

// emptyEnabledReader 让这次预览一条启用规则都没有，**同时**留一类未评估。
//
// 两者要一起出现，否则这条用例守不住顺序：缺失只在推送范围内算，一条规则
// 都不推时它本来就是空的，门禁排在前面还是后面结果一样。而未评估是集群级
// 的、与推不推无关 —— 只有它在场，"哪一句先说"才有区别。
type emptyEnabledReader struct {
	store.Reader
}

func (r emptyEnabledReader) PolicyPreview(
	ctx context.Context, clusterID, namespace string, w store.TimeWindow,
) (store.PolicyPreview, error) {
	pv, err := r.Reader.PolicyPreview(ctx, clusterID, namespace, w)
	if err != nil {
		return pv, err
	}
	pv.Overridden.Enabled = nil
	pv.NotAssessedBaselines = []baseline.Kind{baseline.KindDNS}
	return pv, nil
}
