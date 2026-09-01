package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/collectstore"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/gitwrite"
	"github.com/imkerbos/Distill/internal/httpapi"
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
	h, _, cookie := buildTestRouterWithLog(t, reader, reg, gv, writer, nil, "ERROR", &logs,
		// 观测早就覆盖了周期：这些用例测的不是学习期门禁。
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

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

func (r notAssessedReader) PolicyPreviewAtGranularity(
	ctx context.Context, clusterID, namespace string, w store.TimeWindow,
	g policygen.Granularity,
) (store.PolicyPreview, error) {
	pv, err := r.Reader.PolicyPreviewAtGranularity(ctx, clusterID, namespace, w, g)
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
	// partner 仍缺 METRICS_SCRAPE（登记那一半没配），门禁据此拒绝。
	if !strings.Contains(msg, "METRICS_SCRAPE") {
		t.Errorf("refusal does not name the missing kind METRICS_SCRAPE: %q", msg)
	}
	// **不再报 LB_HEALTH_CHECK**：partner 由一个 LoadBalancer/NodePort Service
	// 暴露，deriveLBHealth 现在直接从它推出健康检查放行（此前只认 Ingress，
	// 会把它误报成缺失、永久卡死这个 namespace）。
	if strings.Contains(msg, "LB_HEALTH_CHECK") {
		t.Errorf("refusal still reports LB_HEALTH_CHECK missing for an LB-exposed namespace: %q", msg)
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

// **本轮不得让原本能推的推不了。** asia 各类齐备（其余 namespace 是不适用，
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
	h, _, cookie := buildTestRouterWithLog(t, f.reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		// 观测早就覆盖了周期：这些用例测的不是学习期门禁。
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

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

func (r injectMissingReader) PolicyPreviewAtGranularity(
	ctx context.Context, clusterID, namespace string, w store.TimeWindow,
	g policygen.Granularity,
) (store.PolicyPreview, error) {
	pv, err := r.Reader.PolicyPreviewAtGranularity(ctx, clusterID, namespace, w, g)
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
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		// 观测早就覆盖了周期：这些用例测的不是学习期门禁。
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

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
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		// 观测早就覆盖了周期：这些用例测的不是学习期门禁。
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

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

func (r emptyEnabledReader) PolicyPreviewAtGranularity(
	ctx context.Context, clusterID, namespace string, w store.TimeWindow,
	g policygen.Granularity,
) (store.PolicyPreview, error) {
	pv, err := r.Reader.PolicyPreviewAtGranularity(ctx, clusterID, namespace, w, g)
	if err != nil {
		return pv, err
	}
	pv.Overridden.Enabled = nil
	pv.NotAssessedBaselines = []baseline.Kind{baseline.KindDNS}
	return pv, nil
}

// 没有流量观测的集群照样要走到门禁，而不是停在「请求参数不合法」。
//
// 这个平台的资产兜底那一轮之后，零流量集群照样产得出候选策略（UAT 实测
// 303 份）。写回请求不带时间窗时，默认窗口答不出来 —— 那**不是**一次参数
// 错误，是「这个集群还没有流量观测」。答成参数错误会把一句关于集群的话
// 变成一次调用方的过错，而操作者会去检查自己的请求。
func TestAClusterWithoutTrafficReachesTheGateNotAParameterError(t *testing.T) {
	f := newWritebackFixture(t)
	f.reader = noWindowReader{Reader: f.reader}
	h, _, cookie := buildTestRouterWithLog(t, f.reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		// 观测早就覆盖了周期：这些用例测的不是学习期门禁。
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("plan succeeded for a cluster with no traffic: %s", rec.Body.String())
	}
	// 拒绝要说得出实质原因（这个 fixture 上是 Enforcing 门禁），而不是
	// 一句「请求参数不合法」—— 后者会让操作者去检查自己的请求。
	if !strings.Contains(msg, "Baseline") && !strings.Contains(msg, "流量") {
		t.Errorf("refused with %q; a cluster with no flow ingest is a fact about the cluster, "+
			"not a mistake by the caller", msg)
	}
}

// noWindowReader 让默认窗口答不出来，模拟一次流量都没摄入过的集群。
type noWindowReader struct {
	store.Reader
}

func (noWindowReader) DefaultWindow(context.Context, string) (store.TimeWindow, error) {
	return store.TimeWindow{}, collectstore.ErrNoFlowIngest
}

// injectUnattachedReader 往"挂不上 workload 的暴露"清单里塞一条。
//
// 包一层而不是改 fixture：合成数据集里每个暴露型 Service 都挂得上，而门禁
// 必须在挂不上的那种形态下挡住 —— 那正是本轮存在的理由。
//
// reason 由调用方给：两种成因门禁的处置**不同**（NO_SUCH_WORKLOAD 挡、
// NO_SELECTOR 不挡），写死一个取值会让另一半没有任何用例。
type injectUnattachedReader struct {
	store.Reader
	namespace string
	name      string
	reason    policygen.UnattachedBaselineReason
}

func (r injectUnattachedReader) PolicyPreviewAtGranularity(
	ctx context.Context, clusterID, namespace string, w store.TimeWindow,
	g policygen.Granularity,
) (store.PolicyPreview, error) {
	pv, err := r.Reader.PolicyPreviewAtGranularity(ctx, clusterID, namespace, w, g)
	if err != nil {
		return pv, err
	}
	pv.UnattachedBaselines = append(pv.UnattachedBaselines, policygen.UnattachedBaselineRule{
		Kind: baseline.KindExposedIngress, Namespace: r.namespace, Name: r.name,
		Reason: r.reason,
	})
	return pv, nil
}

// unattachedReader 是"挡得住的那种成因"的默认替身。
func unattachedReader(inner store.Reader, namespace string) injectUnattachedReader {
	return injectUnattachedReader{
		Reader: inner, namespace: namespace, name: "orphan-lb",
		reason: policygen.UnattachedBaselineNoSuchWorkload,
	}
}

// 挂不上 workload 的暴露必须挡住写回。
//
// MissingBaselines 是 kind 粒度的：这个 namespace 里另一个 Service 正常挂上
// 了，EXPOSED_INGRESS 就算"齐备"，门禁于是放行 —— 而 orphan-lb 背后的
// workload 拿到的是 policyTypes:[Ingress] 加零条放行，集群一个真实的对外
// 入口在 Argo 合并之后无声中断。这正是本轮要防的那件事，而它偏偏从唯一
// 拦得住它的那道门走了过去（spec §6.2）。
func TestPlanIsRefusedWhenAPushedExposureCannotAttach(t *testing.T) {
	f := newWritebackFixture(t)
	reader := unattachedReader(f.reader, "payment")
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("一条挂不上 workload 的对外暴露没有挡住计划: %s", rec.Body.String())
	}
	if !strings.Contains(msg, "orphan-lb") {
		t.Errorf("拒绝文案没有点名那个 Service: %q", msg)
	}
	if f.writer.calls != 0 {
		t.Error("被拒的计划仍然写到了仓库")
	}
}

// 同一道门必须挡住推送：计划出不来的集群，推送更不该有路径进去。
func TestPushIsRefusedWhenAPushedExposureCannotAttach(t *testing.T) {
	f := newWritebackFixture(t)
	reader := unattachedReader(f.reader, "payment")
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

	rec := authedPostJSON(t, h, cookie, writebackPushPath, map[string]any{
		"branch": "distill/prod-asia-1-20260818T090000Z", "fingerprint": "whatever",
	})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("挂不上的暴露没有挡住推送: %s", rec.Body.String())
	}
	// 必须是**门禁**拒的，不是指纹对不上拒的：后者会让这条用例在门禁被删掉
	// 之后照样绿。门禁排在指纹比对之前，因此文案里必须有那个 Service。
	if !strings.Contains(msg, "orphan-lb") {
		t.Errorf("推送被拒了，但不是这道门拒的: %q", msg)
	}
	if f.writer.calls != 0 {
		t.Error("被拒的推送仍然写到了仓库")
	}
}

// 判的是这次真的会被写进文件的那些 namespace，与 missingInPushedNamespaces
// 同一条纪律：没有策略落进去的 namespace 不会获得 default-deny，也就不会被
// 打断；把它算进来等于让一个与本次推送无关的缺口永久挡住所有推送，而一道
// 永远在挡的门会被整体绕开。
func TestAnUnattachedExposureOutsideTheFileDoesNotBlockThePush(t *testing.T) {
	f := newWritebackFixture(t)
	// quarantine 不在候选策略里 —— 这份文件不会给它下发任何东西。
	reader := unattachedReader(f.reader, "quarantine")
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code != 0 {
		t.Fatalf("计划被一个它根本不写入的 namespace 拒了: %s", msg)
	}
}

// **NO_SELECTOR 不挡写回，但必须仍然看得见。**
//
// 没有 spec.selector 的 LoadBalancer / NodePort 是手工维护 Endpoints 的
// 外部后端，spec §6.2 与 derive_exposed.go 都写着它"合法且常见"：这里根本
// 没有 workload 可挂，也没有任何一处改动能让它挂上。挡住它等于给这个
// namespace 装一把没有钥匙的锁 —— 操作者唯一的出路是把这个 namespace 的
// 策略全部禁用到它掉出推送范围，而那正是按推送范围裁剪本来要防的事：一道
// 永远在挡的门会被整体绕开，连同它本该挡住的 NO_SUCH_WORKLOAD 一起
// （design review RI1，2026-08-28）。
func TestASelectorlessExposureDoesNotBlockTheWriteback(t *testing.T) {
	f := newWritebackFixture(t)
	reader := injectUnattachedReader{
		Reader: f.reader, namespace: "payment", name: "external-backend",
		reason: policygen.UnattachedBaselineNoSelector,
	}
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code != 0 {
		t.Fatalf("一个没有 spec.selector 的 Service 挡住了写回，而操作者修不了它: %s", msg)
	}
}

// 不挡不等于不说。这一条与上面那条**必须成对**：只有上面一条时，一个
// "干脆把 NO_SELECTOR 从清单里删掉"的实现照样绿 —— 而那会让一次真实的
// 对外暴露彻底消失，正是 spec §6.2 存在的理由。
func TestASelectorlessExposureIsStillReportedInThePreview(t *testing.T) {
	f := newWritebackFixture(t)
	reader := injectUnattachedReader{
		Reader: f.reader, namespace: "payment", name: "external-backend",
		reason: policygen.UnattachedBaselineNoSelector,
	}
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

	rec := authedGet(t, h, cookie, previewPath)
	var env struct {
		Data struct {
			UnattachedBaselines []policygen.UnattachedBaselineRule `json:"unattachedBaselines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	var found bool
	for _, u := range env.Data.UnattachedBaselines {
		if u.Namespace == "payment" && u.Name == "external-backend" &&
			u.Reason == policygen.UnattachedBaselineNoSelector {
			found = true
		}
	}
	if !found {
		t.Errorf("门禁放行了这条暴露，预览里也没有它 —— 一个真实的对外入口就此无声无息: %+v",
			env.Data.UnattachedBaselines)
	}
}

// 混着来时只报挡得住的那一条：把修不了的那条也写进拒绝文案，操作者会先去
// 追一个没有解法的名字，而真正该改的那个 Service 排在它后面。
func TestTheRefusalNamesOnlyTheFixableUnattachedExposure(t *testing.T) {
	f := newWritebackFixture(t)
	reader := injectUnattachedReader{
		Reader: unattachedReader(f.reader, "payment"),
		// 与上面那条同一个 namespace，成因不同。
		namespace: "payment", name: "external-backend",
		reason: policygen.UnattachedBaselineNoSelector,
	}
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("挂得上却错配了标签的那条暴露没有挡住计划: %s", rec.Body.String())
	}
	if !strings.Contains(msg, "orphan-lb") {
		t.Errorf("拒绝文案没有点名那个改得动的 Service: %q", msg)
	}
	if strings.Contains(msg, "external-backend") {
		t.Errorf("拒绝文案点名了一个操作者改不了的 Service: %q", msg)
	}
}

// 拒绝文案里的处置必须是**这一栏真的走得通的那条**。
//
// 此前整段文案共用一句"若某一类在本集群确实不需要，请在集群登记里写下理由"
// —— 那条出路只对 kind 粒度的缺失成立（NoNodeAgentsReason）。被一条挂不上的
// 暴露挡住的操作者照着做，会在集群登记里找一个不存在的字段，而真正该做的
// （对齐 Service selector 与 workload 标签）一个字都没说。
func TestTheUnattachedRefusalPointsAtTheLabelsNotTheRegistry(t *testing.T) {
	f := newWritebackFixture(t)
	reader := unattachedReader(f.reader, "payment")
	h, _, cookie := buildTestRouterWithLog(t, reader, f.reg, f.verifier, f.writer, nil, "ERROR", f.logs,
		&stubObservedSince{observedSince: time.Now().Add(-365 * 24 * time.Hour)})

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	code, msg := refusal(t, rec.Body.String())
	if code == 0 {
		t.Fatalf("挂不上的暴露没有挡住计划: %s", rec.Body.String())
	}
	if !strings.Contains(msg, "selector") {
		t.Errorf("拒绝没有说该去改什么: %q", msg)
	}
	if strings.Contains(msg, "若某一类在本集群确实不需要") {
		t.Errorf("拒绝把操作者指向了集群登记，而那里没有能豁免这一条的地方: %q", msg)
	}
}

// **挂不上的探针要按探针的话说，不能套用 Service 那套处置。**
//
// 这条拦截的文案原本只服务 EXPOSED_INGRESS —— 那时 Subject 非空的 Baseline
// 只有它一类。KUBELET_PROBE 也带 Subject 之后，同一句话会把一条探针缺口
// 描述成"对外暴露的 Service"，并让操作者去对齐一个根本不存在的 Service 的
// spec.selector。处置指错方向比不给处置更糟：他会去翻一个不存在的对象，
// 而真正要看的是这个 workload 的归属标签。
func TestTheGateDescribesAnUnattachedProbeInItsOwnTerms(t *testing.T) {
	msg := httpapi.EnforcingBlockersForTest(store.PolicyPreview{
		Overridden: store.OverriddenView{
			Enabled: []networkingv1.NetworkPolicy{
				{ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "w"}},
			},
		},
		UnattachedBaselines: []policygen.UnattachedBaselineRule{{
			Kind: baseline.KindKubeletProbe, Namespace: "monitoring",
			Name: "prometheus-node-exporter", Reason: policygen.UnattachedBaselineNoSuchWorkload,
		}},
	})
	if msg == "" {
		t.Fatal("挂不上的探针没有挡住写回")
	}
	if !strings.Contains(msg, "prometheus-node-exporter") {
		t.Errorf("没点名是哪个 workload: %s", msg)
	}
	if strings.Contains(msg, "spec.selector") || strings.Contains(msg, "对外暴露的 Service") {
		t.Errorf("把探针缺口说成了 Service 的事，处置指向一个不存在的对象:\n%s", msg)
	}
}

// EXPOSED_INGRESS 那一支的文案不变：它说的确实是 Service。
func TestTheGateStillDescribesAnUnattachedExposureAsAService(t *testing.T) {
	msg := httpapi.EnforcingBlockersForTest(store.PolicyPreview{
		Overridden: store.OverriddenView{
			Enabled: []networkingv1.NetworkPolicy{
				{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "w"}},
			},
		},
		UnattachedBaselines: []policygen.UnattachedBaselineRule{{
			Kind: baseline.KindExposedIngress, Namespace: "shop",
			Name: "orphan-lb", Reason: policygen.UnattachedBaselineNoSuchWorkload,
		}},
	})
	if !strings.Contains(msg, "spec.selector") {
		t.Errorf("暴露那一支丢了它自己的处置: %s", msg)
	}
}
