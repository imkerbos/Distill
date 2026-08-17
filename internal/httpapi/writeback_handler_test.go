package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/gitwrite"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

const (
	writebackPlanPath = "/api/v1/clusters/prod-asia-1/policy-writeback/plan"
	writebackPushPath = "/api/v1/clusters/prod-asia-1/policy-writeback/push"
	// writebackPolicyPath 是绑定里那条策略根路径，写回的文件必须落在它之内。
	writebackPolicyPath = "clusters/prod-asia-1"
	// pushedCommitSHA 是替身写入器报出来的 commit。四十位十六进制 ——
	// registry.ValidateCommitSHA 不接受缩写。
	pushedCommitSHA = "0123456789abcdef0123456789abcdef01234567"
	// writebackFilePath 是这次写回的落点。写成字面量而不是引用 handler 里
	// 那个常量（包外测试也引用不到）：多余文件的判定要拿它与仓库现状比，
	// 而"平台要写的那个文件不算多余"这条，靠的正是两边说的是同一条路径。
	writebackFilePath = writebackPolicyPath + "/distill-policy.yaml"
)

// 替身必须同时满足两个接口，与 mysqlregistry.Store 一样。编译期断言而不是
// 靠某条用例碰巧调到：接口漏实现应当在构建时失败。
var _ registry.WritebackStore = (*memRegistry)(nil)

// memWriteback 是替身记下的一次写回审计。
//
// 记整条记录而不是一个计数：一条只数次数的断言，对一次把分支、文件数或
// 四类计数记错的写回照样是绿的。
type memWriteback struct {
	actor     string
	clusterID string
	// commit 只在推送那一支有值：出计划阶段还没有 commit。
	commit    string
	writeback registry.Writeback
}

// RecordWritebackPlan 记一条出计划的审计。
//
// 与 SetLastWrittenCommit 分别记进两个切片，而不是一个带 action 字段的
// 列表：「出计划与推送是两个动作」这条（design doc 2026-08-14 §9）只有在
// 两者分得开时才断言得了，而一个共用的列表会让把两者写成同一个动作的实现
// 照样通过。
func (m *memRegistry) RecordWritebackPlan(
	_ context.Context, actor registry.Actor, clusterID string, w registry.Writeback,
) error {
	m.record("RecordWritebackPlan")
	if err := m.writeErr(); err != nil {
		return err
	}
	if actor.Username == "" {
		return registry.NewInvalidError("审计缺少操作者")
	}
	// 与真实实现一样先过校验：替身若比它宽松，handler 测试会在一条真实
	// 实现会拒绝的输入上通过。
	if err := registry.ValidateWriteback(w); err != nil {
		return err
	}
	m.writebackPlans = append(m.writebackPlans,
		memWriteback{actor: actor.Username, clusterID: clusterID, writeback: w})
	return nil
}

// SetLastWrittenCommit 记一条推送审计，并把 commit 落到绑定上。
//
// 真实实现里这两件事在同一个事务里（design doc §8）；替身没有事务，但
// 一起做还是一起不做这条必须保持，否则「推了但没记」在测试里看不出来。
func (m *memRegistry) SetLastWrittenCommit(
	_ context.Context, actor registry.Actor, clusterID, commitSHA string, w registry.Writeback,
) error {
	m.record("SetLastWrittenCommit")
	if err := m.writeErr(); err != nil {
		return err
	}
	if actor.Username == "" {
		return registry.NewInvalidError("审计缺少操作者")
	}
	if err := registry.ValidateCommitSHA(commitSHA); err != nil {
		return err
	}
	if err := registry.ValidateWriteback(w); err != nil {
		return err
	}
	c, ok := m.clusters[clusterID]
	if !ok || c.Git == nil {
		return registry.ErrNotFound
	}
	g := *c.Git
	g.LastWrittenCommit = commitSHA
	c.Git = &g
	m.clusters[clusterID] = c
	m.writebackPushes = append(m.writebackPushes,
		memWriteback{actor: actor.Username, clusterID: clusterID, commit: commitSHA, writeback: w})
	return nil
}

// writebackStoreOf 把注册表替身当作写回存储交给路由。
//
// 断言不成立时返回 nil 接口，而不是一个包着 nil 的接口值：handler 用
// `d.Writeback == nil` 判断"没有审计去处"，包着 nil 的接口会让那个判断为假，
// 随后在空指针上调方法。
func writebackStoreOf(reg registry.Store) registry.WritebackStore {
	wb, ok := reg.(registry.WritebackStore)
	if !ok {
		return nil
	}
	return wb
}

// fakePolicyWriter 是 httpapi.PolicyWriter 的替身。
//
// 记调用次数与看到的计划：本组用例里最重要的一类断言是「这次请求什么都
// 没写」，而那只有在写入器自己数得出被调过几次时才成立 —— 一条只看响应码
// 的断言，对一个"先推出去再返回错误"的实现是绿的。
type fakePolicyWriter struct {
	calls    int
	seenRepo registry.GitRepo
	seenPlan registry.WritebackPlan
	// err 非 nil 时这次推送失败，且**不**记入 calls 之外的任何状态。
	err error

	// listing 是替身报出来的仓库现状：策略路径下已有的文件与现存的
	// distill/* 分支。计划里那两份清单必须原样来自它 —— 由 handler 自己
	// 编一份，界面上那句"（无）多余文件"就又成了没有人算过的断言。
	listing gitwrite.RepoListing
	// listErr 非 nil 时枚举失败。此时**不得**出计划：清单留空在界面上读
	// 起来是"仓库里没有多余文件"。
	listErr error
	// listCalls 数枚举被调过几次，listedRepo/listedPath 记它被问的是哪个
	// 仓库的哪条策略路径。
	listCalls  int
	listedRepo registry.GitRepo
	listedPath string
}

func (f *fakePolicyWriter) List(
	_ context.Context, repo registry.GitRepo, policyPath string,
) (gitwrite.RepoListing, error) {
	f.listCalls++
	f.listedRepo = repo
	f.listedPath = policyPath
	if f.listErr != nil {
		return gitwrite.RepoListing{}, f.listErr
	}
	return f.listing, nil
}

func (f *fakePolicyWriter) Push(
	_ context.Context, repo registry.GitRepo, plan registry.WritebackPlan,
) (string, error) {
	f.calls++
	f.seenRepo = repo
	f.seenPlan = plan
	if f.err != nil {
		return "", f.err
	}
	return pushedCommitSHA, nil
}

// writebackFixture 是一次写回用例的完整装配。
type writebackFixture struct {
	h        http.Handler
	cookie   *http.Cookie
	reg      *memRegistry
	reader   store.Reader
	verifier *stubGitVerifier
	writer   *fakePolicyWriter
	logs     *bytes.Buffer
}

// newWritebackFixture 装一个绑定了策略仓库、校验器结论为 OK 的集群。
//
// reader 与 handler 共用**同一个**注册表：写回的计数取的是应用人工决定
// 之后那一套，两边各拿一个注册表的话，两套计算恒等，「计数取自哪一套」
// 就没有东西守得住（与导出那条用例同一个理由）。
//
// 校验器结论默认 OK：仓库级不是 OK 时写回一律拒绝，而那条拒绝正是本组
// 里要能被单独打开的开关 —— 默认就关着的话，其余每条用例都停在它上面。
func newWritebackFixture(t *testing.T) *writebackFixture {
	t.Helper()

	reg := fixtureSource()
	reg.repos[gitRepoID] = boundRepo()
	c := reg.clusters["prod-asia-1"]
	c.Git = &registry.GitBinding{
		RepoID: gitRepoID, PolicyPath: writebackPolicyPath,
		VerifyResult: registry.BindingVerifyNotVerified,
	}
	reg.clusters["prod-asia-1"] = c

	reader := store.NewFixtureReader(fixture.Load(), reg)
	gv := &stubGitVerifier{result: registry.BindingVerifyOK}
	// 仓库现状默认非空：策略目录下已经有一个平台不会碰的文件，仓库上还攒着
	// 一条没人合的 distill 分支。两份清单都为空的话，"计划是否真的报出了它们"
	// 根本区分不出来 —— 一个恒返回空清单的实现照样绿。
	writer := &fakePolicyWriter{listing: gitwrite.RepoListing{
		Files: []string{
			writebackPolicyPath + "/legacy.yaml",
			writebackFilePath,
		},
		Branches: []string{"distill/prod-asia-1-20260801T090000Z"},
	}}
	var logs bytes.Buffer
	h, _, cookie := buildTestRouterWithLog(t, reader, reg, gv, writer, nil, "ERROR", &logs)

	return &writebackFixture{
		h: h, cookie: cookie, reg: reg, reader: reader,
		verifier: gv, writer: writer, logs: &logs,
	}
}

// planView 是计划响应里本组用例要读的部分。
//
// 手写这几个字段而不是复用 registry.WritebackPlan：断言的是**真的传给
// 前端的那份 JSON**。拿被测类型自己去解自己的输出，一个字段被改成不
// 序列化也照样绿。
type planView struct {
	Plan struct {
		Files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"files"`
		RepoID           string         `json:"repoId"`
		Branch           string         `json:"branch"`
		CommitMessage    string         `json:"commitMessage"`
		Counts           map[string]int `json:"counts"`
		Extraneous       []string       `json:"extraneous"`
		ExistingBranches []string       `json:"existingBranches"`
		Fingerprint      string         `json:"fingerprint"`
	} `json:"plan"`
	RepoVerifyResult    string `json:"repoVerifyResult"`
	BindingVerifyResult string `json:"bindingVerifyResult"`
}

// fetchPlan 出一次计划，要求成功。
func fetchPlan(t *testing.T, f *writebackFixture) planView {
	t.Helper()
	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath, map[string]any{})
	var env struct {
		Code int      `json:"code"`
		Data planView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode plan: %v (%s)", err, rec.Body.String())
	}
	if env.Code != 0 {
		t.Fatalf("plan code = %d, want 0 (%s)", env.Code, rec.Body.String())
	}
	if env.Data.Plan.Fingerprint == "" {
		t.Fatal("plan carries no fingerprint — nothing could ever be confirmed against it")
	}
	return env.Data
}

// 缺省行为必须是安全的：不带指纹的请求永远不写（design doc §5）。
//
// 三种"没带指纹"的写法各试一遍，且**同一条用例里紧跟着一次带指纹的成功
// 推送**：一个"一律拒绝"的实现会让上半段永远通过，而那等于把这个功能整个
// 关掉 —— 那种实现同样满足"没带指纹就没写"，却什么都做不了。
func TestPushWithoutAFingerprintWritesNothing(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"空请求体", map[string]any{}},
		{"只带分支", map[string]any{"branch": plan.Plan.Branch}},
		{"指纹是空串", map[string]any{"branch": plan.Plan.Branch, "fingerprint": "   "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath, c.body)
			if got := bodyOf(t, rec)["code"]; got != float64(20001) {
				t.Fatalf("code = %v, want 20001 (%s)", got, rec.Body.String())
			}
			if f.writer.calls != 0 {
				t.Fatalf("a request without a fingerprint reached the writer %d times", f.writer.calls)
			}
			if len(f.reg.writebackPushes) != 0 {
				t.Fatalf("a request without a fingerprint wrote %d push audit rows",
					len(f.reg.writebackPushes))
			}
			if got := f.reg.clusters["prod-asia-1"].Git.LastWrittenCommit; got != "" {
				t.Fatalf("last_written_commit = %q after a refused push", got)
			}
		})
	}

	// 带上指纹仍然推得出去 —— 这条守的是"守卫没有把功能一起关掉"。
	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": plan.Plan.Branch, "fingerprint": plan.Plan.Fingerprint})
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("a confirmed push was refused: %s", rec.Body.String())
	}
	if f.writer.calls != 1 {
		t.Fatalf("writer calls = %d, want exactly 1", f.writer.calls)
	}
}

// 指纹对不上就拒绝：确认的必须是操作者真正看过的那一份，不是"当时那一份
// 的后续版本"（design doc §4）。
//
// 让它真的过期，而不是随手编一串十六进制：编出来的指纹只能证明"不等就拒"，
// 证明不了"平台在推之前真的重算过"。这里在两次调用之间加一条人工决定 ——
// 候选集与四类计数都变了，那份旧计划描述的于是确实是另一套东西。
func TestStalePlanFingerprintIsRefused(t *testing.T) {
	f := newWritebackFixture(t)
	stale := fetchPlan(t, f)

	ns, wl, fp := singleSidedDisabledRule(t, fetchPreview(t, f.h, f.cookie, ""))
	create := authedPostJSON(t, f.h, f.cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "确认之后再推，计划应当作废"})
	if got := bodyOf(t, create)["code"]; got != float64(0) {
		t.Fatalf("create override code = %v (%s)", got, create.Body.String())
	}

	fresh := fetchPlan(t, f)
	// 先决条件：两份计划确实不同，否则下面那条断言无法失败。
	if fresh.Plan.Fingerprint == stale.Plan.Fingerprint {
		t.Fatal("the plan did not change after a confirmation — this test cannot tell the two apart")
	}

	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": stale.Plan.Branch, "fingerprint": stale.Plan.Fingerprint})
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 for a stale fingerprint (%s)", got, rec.Body.String())
	}
	if f.writer.calls != 0 {
		t.Fatalf("a stale plan reached the writer %d times", f.writer.calls)
	}
	if len(f.reg.writebackPushes) != 0 {
		t.Fatalf("a stale plan wrote %d push audit rows", len(f.reg.writebackPushes))
	}

	// 新指纹推得出去：拒绝的是"过期"，不是"推送"这件事。
	ok := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": fresh.Plan.Branch, "fingerprint": fresh.Plan.Fingerprint})
	if got := bodyOf(t, ok)["code"]; got != float64(0) {
		t.Fatalf("the fresh plan was refused as well: %s", ok.Body.String())
	}
	if f.writer.calls != 1 {
		t.Fatalf("writer calls = %d, want exactly 1", f.writer.calls)
	}
}

// 写之前重新校验绑定：仓库级结论不是 OK 就拒绝写（design doc §4）。
//
// 绑定上那个 verified_at 是历史事实，不是当前状态（2026-08-13 spec §3.4）。
// 这里让绑定在出计划之后失效 —— 凭据被轮换、权限被收回都是这个形状。
func TestPushRefusesWhenTheBindingNoLongerVerifies(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)
	if plan.RepoVerifyResult != string(registry.RepoVerifyOK) {
		t.Fatalf("plan repoVerifyResult = %q, want OK — the fixture is not in the state this test needs",
			plan.RepoVerifyResult)
	}
	callsAfterPlan := f.verifier.repoCalls
	if callsAfterPlan == 0 {
		t.Fatal("planning never re-verified the binding")
	}

	// 库里那条结论仍然是刚才那个 OK：拒绝必须来自**这一刻**的重新校验，
	// 不是来自库里存着的什么。
	f.verifier.repoResult = registry.RepoVerifyAuthFailed

	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": plan.Plan.Branch, "fingerprint": plan.Plan.Fingerprint})
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 (%s)", got, rec.Body.String())
	}
	if f.verifier.repoCalls <= callsAfterPlan {
		t.Fatalf("repo verify calls = %d, unchanged since planning — the push never re-verified",
			f.verifier.repoCalls)
	}
	if f.writer.calls != 0 {
		t.Fatalf("a push on an unverified binding reached the writer %d times", f.writer.calls)
	}
	if len(f.reg.writebackPushes) != 0 {
		t.Fatalf("a refused push wrote %d push audit rows", len(f.reg.writebackPushes))
	}
	if got := f.reg.clusters["prod-asia-1"].Git.LastWrittenCommit; got != "" {
		t.Fatalf("last_written_commit = %q after a refused push", got)
	}

	// 结论回到 OK 之后同一份计划推得出去：拒绝的是"没通过校验"，不是
	// "这条路径"。
	f.verifier.repoResult = registry.RepoVerifyOK
	ok := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": plan.Plan.Branch, "fingerprint": plan.Plan.Fingerprint})
	if got := bodyOf(t, ok)["code"]; got != float64(0) {
		t.Fatalf("the same plan was refused with a verified binding: %s", ok.Body.String())
	}
}

// 计数由平台在写前重算，请求体里的数字一律不被采纳（design doc §4）。
//
// 三个方向同时钉住：
//
//  1. 计划与审计里的四类计数等于**预览响应里那一套 overridden 计数** ——
//     操作者屏幕上的那几个数；
//  2. 请求体里塞进一套完全不同的数字，落库与回传的仍然是平台算出来的那套；
//  3. 先决地要求 overridden 与默认那一套不同，否则"取的是哪一套"根本
//     区分不出来 —— 这正是轮 3 那条 Critical 的形状。
func TestCountsComeFromTheServerNotTheRequest(t *testing.T) {
	f := newWritebackFixture(t)

	ns, wl, fp := singleSidedDisabledRule(t, fetchPreview(t, f.h, f.cookie, ""))
	create := authedPostJSON(t, f.h, f.cookie, "/api/v1/clusters/prod-asia-1/rule-overrides",
		map[string]any{"namespace": ns, "workload": wl, "fingerprint": fp,
			"decision": "ENABLE", "reason": "写回必须带上这条确认"})
	if got := bodyOf(t, create)["code"]; got != float64(0) {
		t.Fatalf("create override code = %v (%s)", got, create.Body.String())
	}

	pv := fetchPreview(t, f.h, f.cookie, "")
	// 先决条件：两套计数确实不同。
	if reflect.DeepEqual(pv.Prediction.Counts, pv.Overridden.Prediction.Counts) {
		t.Fatalf("both computations report the same counts %v — this test cannot tell them apart",
			pv.Prediction.Counts)
	}
	want := map[string]int{}
	for _, k := range predict.AllChangeKinds() {
		want[string(k)] = pv.Overridden.Prediction.Counts[k]
	}

	plan := fetchPlan(t, f)
	if !reflect.DeepEqual(plan.Plan.Counts, want) {
		t.Errorf("plan counts = %v, the report the operator read says %v", plan.Plan.Counts, want)
	}

	// 请求体自述一套影响面。它一个字都不该被采纳 —— 一个能描述自己那次
	// 变更爆炸半径的调用方，等于把 dry-run 这件事交给了被评估的一方。
	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath, map[string]any{
		"branch":      plan.Plan.Branch,
		"fingerprint": plan.Plan.Fingerprint,
		"counts": map[string]int{
			"WOULD_BREAK": 0, "WOULD_OPEN": 0, "UNCHANGED": 9999, "UNKNOWN": 0,
		},
		"files": 42,
	})
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("push code = %v (%s)", got, rec.Body.String())
	}
	if len(f.reg.writebackPushes) != 1 {
		t.Fatalf("push audit rows = %d, want 1", len(f.reg.writebackPushes))
	}
	got := f.reg.writebackPushes[0].writeback
	gotCounts := map[string]int{}
	for k, v := range got.Counts {
		gotCounts[string(k)] = v
	}
	if !reflect.DeepEqual(gotCounts, want) {
		t.Errorf("audited counts = %v, want %v — the request body was believed", gotCounts, want)
	}
	if got.Files != 1 {
		t.Errorf("audited file count = %d, want 1 — the request body was believed", got.Files)
	}
	// 提交信息里那几行同样是重算出来的：它是评审人唯一会读的那句话。
	for _, k := range predict.AllChangeKinds() {
		line := fmt.Sprintf("dry-run %s: %d", k, want[string(k)])
		if !strings.Contains(plan.Plan.CommitMessage, line) {
			t.Errorf("commit message missing %q:\n%s", line, plan.Plan.CommitMessage)
		}
	}
}

// 出计划与推送写不同的审计动作（design doc §9）。
//
// 混成一条会让"谁真的改了仓库"淹没在计划请求里 —— 而事故复盘时要问的
// 恰恰是前者。这里出两次计划、推一次，断言两边各自的条数与内容：一个把
// 两者写成同一个动作的实现会让其中一边多出两条。
func TestPlanAndPushAreDistinctAuditActions(t *testing.T) {
	f := newWritebackFixture(t)

	first := fetchPlan(t, f)
	fetchPlan(t, f)
	if len(f.reg.writebackPlans) != 2 {
		t.Fatalf("plan audit rows = %d, want 2", len(f.reg.writebackPlans))
	}
	if len(f.reg.writebackPushes) != 0 {
		t.Fatalf("planning wrote %d push audit rows — a plan writes nothing to the repository",
			len(f.reg.writebackPushes))
	}

	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": first.Plan.Branch, "fingerprint": first.Plan.Fingerprint})
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("push code = %v (%s)", got, rec.Body.String())
	}

	if len(f.reg.writebackPlans) != 2 {
		t.Errorf("plan audit rows = %d after a push, want 2 — the push was recorded as a plan",
			len(f.reg.writebackPlans))
	}
	if len(f.reg.writebackPushes) != 1 {
		t.Fatalf("push audit rows = %d, want 1", len(f.reg.writebackPushes))
	}
	push := f.reg.writebackPushes[0]
	if push.commit != pushedCommitSHA {
		t.Errorf("audited commit = %q, want the SHA the platform pushed (%q)", push.commit, pushedCommitSHA)
	}
	if push.actor != "demo" || push.clusterID != "prod-asia-1" {
		t.Errorf("audited actor/cluster = %q/%q, want demo/prod-asia-1", push.actor, push.clusterID)
	}
	if push.writeback.Branch != first.Plan.Branch {
		t.Errorf("audited branch = %q, want %q", push.writeback.Branch, first.Plan.Branch)
	}
	// 出计划那两条不带 commit：一个凭空出现的 SHA 会让读的人以为那次计划
	// 已经推出去了。
	for i, p := range f.reg.writebackPlans {
		if p.commit != "" {
			t.Errorf("plan audit row %d carries commit %q", i, p.commit)
		}
	}
	// 推送成功后写 last_written_commit（design doc §8）：记的是平台交出去的
	// 那个 commit，不是合并后的。
	if got := f.reg.clusters["prod-asia-1"].Git.LastWrittenCommit; got != pushedCommitSHA {
		t.Errorf("last_written_commit = %q, want %q", got, pushedCommitSHA)
	}
}

// 出计划不写任何东西：它是写回的 dry-run，也是默认形态（design doc §5）。
func TestWritebackPlanNeverReachesTheRepository(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	if f.writer.calls != 0 {
		t.Errorf("planning reached the writer %d times", f.writer.calls)
	}
	if got := f.reg.clusters["prod-asia-1"].Git.LastWrittenCommit; got != "" {
		t.Errorf("last_written_commit = %q after a plan", got)
	}
	if len(plan.Plan.Files) != 1 {
		t.Fatalf("plan has %d files, want exactly 1", len(plan.Plan.Files))
	}
	// 落点必须在绑定的 policyPath 之内 —— 一份能写出 policyPath 之外的
	// 计划，等于让平台改写仓库里任意文件。
	if !strings.HasPrefix(plan.Plan.Files[0].Path, writebackPolicyPath+"/") {
		t.Errorf("file path %q is not inside the binding's policyPath %q",
			plan.Plan.Files[0].Path, writebackPolicyPath)
	}
	// 目标分支永远是新建的 distill/*，不是绑定里那条部署分支（§2）。
	if !strings.HasPrefix(plan.Plan.Branch, "distill/") {
		t.Errorf("branch = %q, want a distill/* branch", plan.Plan.Branch)
	}
	if plan.Plan.Branch == boundRepo().Branch {
		t.Errorf("branch = %q — that is the branch Config Sync applies", plan.Plan.Branch)
	}
}

// 计划必须报出仓库里多余的文件与现存的 distill/* 分支，且两份都来自平台
// **真的枚举过一次仓库**（design doc §2、§3、§4）。
//
// 界面无条件渲染这两份清单：一份恒为空的清单在屏幕上不是"空集"，是一句
// "仓库里没有多余文件、也没有攒着的分支"的事实陈述 —— 而没有枚举的话，
// 平台从没算过这件事，且偏在让人放心的方向。
//
// 正反两向都钉：多余的那个文件要在，平台自己这次要写的那个文件不能在
// （它不是多余的），而"这次枚举问的是这个仓库的这条策略路径"同样断言到 ——
// 一份问错路径的枚举会报出一整个目录的假多余文件。
func TestPlanReportsWhatTheRepositoryAlreadyHas(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	if f.writer.listCalls == 0 {
		t.Fatal("planning never enumerated the repository —— 计划里那两份清单没有出处")
	}
	if f.writer.listedPath != writebackPolicyPath {
		t.Errorf("listing asked for policy path %q, want %q", f.writer.listedPath, writebackPolicyPath)
	}
	if f.writer.listedRepo.ID != boundRepo().ID {
		t.Errorf("listing asked repo %q, want the bound repo %q", f.writer.listedRepo.ID, boundRepo().ID)
	}

	if want := []string{writebackPolicyPath + "/legacy.yaml"}; !reflect.DeepEqual(
		plan.Plan.Extraneous, want) {
		t.Errorf("plan extraneous = %v, want %v —— 多余文件的清单不是这次枚举的结果",
			plan.Plan.Extraneous, want)
	}
	// 平台这次要写的那个文件不是"多余"的：把它列进去，操作者会去删掉一份
	// 平台正要更新的策略。
	for _, p := range plan.Plan.Extraneous {
		if p == writebackFilePath {
			t.Errorf("the file this write-back is about (%q) was listed as extraneous", p)
		}
	}
	if want := []string{"distill/prod-asia-1-20260801T090000Z"}; !reflect.DeepEqual(
		plan.Plan.ExistingBranches, want) {
		t.Errorf("plan existingBranches = %v, want %v —— 攒着没人合的分支这个信号没有报出来",
			plan.Plan.ExistingBranches, want)
	}
}

// 枚举失败时整次不出计划（design doc §4）。
//
// 失败方向必须朝关：一份清单留空的计划在界面上会说"仓库里没有多余文件"，
// 而那句话平台并没有算过。这条同时断言错误正文不进响应也不进日志 ——
// go-git 的报错带着仓库路径、主机名与传输细节（规范 §19、§21、§22）。
func TestPlanIsRefusedWhenTheRepositoryCannotBeListed(t *testing.T) {
	f := newWritebackFixture(t)
	const raw = "ssh: handshake failed for git@gitlab.internal.example:2222/net/policies.git"
	f.writer.listErr = fmt.Errorf("list: %s", raw)

	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath, map[string]any{})
	body := bodyOf(t, rec)
	if got := body["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 (%s)", got, rec.Body.String())
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "没能列出") {
		t.Errorf("msg = %q, want the reason the plan was withheld", msg)
	}
	if strings.Contains(rec.Body.String(), "distill/") {
		t.Errorf("a plan was served despite the listing failing: %s", rec.Body.String())
	}
	for _, leak := range []string{"gitlab.internal.example", "handshake", "2222", raw} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the response leaked %q: %s", leak, rec.Body.String())
		}
		if strings.Contains(f.logs.String(), leak) {
			t.Errorf("the log leaked %q: %s", leak, f.logs.String())
		}
	}
	if len(f.reg.writebackPlans) != 0 {
		t.Errorf("a withheld plan wrote %d audit rows", len(f.reg.writebackPlans))
	}

	// 枚举恢复之后计划照出：拒绝的是"没枚举成"，不是"出计划"这件事。
	f.writer.listErr = nil
	fetchPlan(t, f)
}

// 计划必须说出它要写到**哪个仓库**，而那一维要进指纹（design doc §4，
// 2026-08-15 修订）。
//
// 不绑定的话，在出计划与推送之间把绑定改指到另一个仓库，操作者读过的每一句
// 话都还成立、指纹照样对得上，推送却落到了另一个仓库 —— 那是他唯一没有明示
// 批准过的一维。这里让绑定在两次调用之间改指，断言旧计划当场作废。
func TestPushRefusesAPlanAfterTheBindingIsPointedAtAnotherRepository(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)
	if plan.Plan.RepoID != gitRepoID {
		t.Fatalf("plan repoId = %q, want the bound repository %q —— 计划没有说出它要写到哪儿",
			plan.Plan.RepoID, gitRepoID)
	}

	// 另一个仓库，其余一切照旧：文件、分支、计数、提交信息都不会变。
	const otherRepoID = "repo-somewhere-else"
	other := boundRepo()
	other.ID = otherRepoID
	f.reg.repos[otherRepoID] = other
	c := f.reg.clusters["prod-asia-1"]
	g := *c.Git
	g.RepoID = otherRepoID
	c.Git = &g
	f.reg.clusters["prod-asia-1"] = c

	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": plan.Plan.Branch, "fingerprint": plan.Plan.Fingerprint})
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 —— 一份批准给另一个仓库的计划被放行了 (%s)",
			got, rec.Body.String())
	}
	if f.writer.calls != 0 {
		t.Errorf("a redirected push reached the writer %d times", f.writer.calls)
	}
	if len(f.reg.writebackPushes) != 0 {
		t.Errorf("a redirected push wrote %d audit rows", len(f.reg.writebackPushes))
	}

	// 对着新仓库重新出一次计划就推得出去：拒绝的是"改了落点"，不是"推送"。
	fresh := fetchPlan(t, f)
	if fresh.Plan.RepoID != otherRepoID {
		t.Fatalf("fresh plan repoId = %q, want %q", fresh.Plan.RepoID, otherRepoID)
	}
	if fresh.Plan.Fingerprint == plan.Plan.Fingerprint {
		t.Error("换了仓库之后指纹没变 —— 内容原封不动地落到另一个仓库也照样通过")
	}
	ok := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": fresh.Plan.Branch, "fingerprint": fresh.Plan.Fingerprint})
	if got := bodyOf(t, ok)["code"]; got != float64(0) {
		t.Fatalf("the freshly planned push was refused: %s", ok.Body.String())
	}
	if f.writer.seenPlan.RepoID != otherRepoID {
		t.Errorf("the writer was handed a plan for %q, want %q", f.writer.seenPlan.RepoID, otherRepoID)
	}
}

// 计划里的文件内容必须**就是导出端点交出去的那份**（design doc §7）。
//
// 另起一段渲染，两份会慢慢分家，而分家之后没有任何东西看得出来：操作者
// 下载下来核对的是一份，推进仓库的是另一份。这里逐字节比 —— 只有注释头里
// 那个时刻不同（导出用导出时刻，写回用出计划的时刻），把那一行剔掉之后
// 必须完全相等。
func TestWritebackFileIsTheSameContentTheExportServes(t *testing.T) {
	f := newWritebackFixture(t)

	plan := fetchPlan(t, f)
	rec := authedGet(t, f.h, f.cookie, exportPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d (%s)", rec.Code, rec.Body.String())
	}

	dropMoment := func(s string) string {
		var kept []string
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "# 导出时刻: ") {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n")
	}
	got, want := dropMoment(plan.Plan.Files[0].Content), dropMoment(rec.Body.String())
	if got != want {
		t.Errorf("the file the platform would write is not the file the export serves:\n--- writeback ---\n%s\n--- export ---\n%s",
			got, want)
	}
	// 剔掉的那一行必须真的在两边都有，否则上面的比较是在比两份都被削掉
	// 一大块的文本。
	for name, body := range map[string]string{
		"writeback": plan.Plan.Files[0].Content, "export": rec.Body.String(),
	} {
		if !strings.Contains(body, "# 导出时刻: ") {
			t.Errorf("%s body has no moment line — this test's comparison is vacuous", name)
		}
	}
}

// 提交信息必须自述，且**只**有这几行（design doc §7）。
//
// 它会永久留在策略仓库的历史里，也是合并请求上的评审人唯一会读的那句话。
// 因此钉的是一份封闭清单，多出来的一行会红，而不是"看看有没有出现某个
// 敏感词" —— 一份关键词黑名单对它没想到的那种泄漏永远是绿的。
func TestWritebackCommitMessageIsSelfDescribingAndNothingMore(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	allowed := []string{
		"policy: distill 写回 ",
		"集群: ",
		"时间窗: ",
		"dry-run WOULD_BREAK: ", "dry-run WOULD_OPEN: ",
		"dry-run UNCHANGED: ", "dry-run UNKNOWN: ",
		"发起者: ",
		"平台版本: ",
		"以上 dry-run 结论算的是出计划那一刻的集群状态",
		"", // 主题与正文之间那个空行
	}
	for _, line := range strings.Split(strings.TrimSuffix(plan.Plan.CommitMessage, "\n"), "\n") {
		known := false
		for _, prefix := range allowed {
			if line == prefix || (prefix != "" && strings.HasPrefix(line, prefix)) {
				known = true
				break
			}
		}
		if !known {
			t.Errorf("commit message line %q is not on the list this test allows —— "+
				"新增一行要先说明它不含凭据、host key 或内部地址", line)
		}
	}
	for _, want := range []string{"集群: prod-asia-1", "发起者: demo"} {
		if !strings.Contains(plan.Plan.CommitMessage, want) {
			t.Errorf("commit message missing %q:\n%s", want, plan.Plan.CommitMessage)
		}
	}
	// 反方向：仓库地址、凭据引用与主机名一个都不许出现在会被推出去的
	// 任何一段文字里（规范 §19、§21）。
	repo := boundRepo()
	for _, leak := range []string{repo.URL, repo.CredentialRef, "gitlab.example.com", "ssh://"} {
		for name, body := range map[string]string{
			"commit message": plan.Plan.CommitMessage,
			"file content":   plan.Plan.Files[0].Content,
		} {
			if strings.Contains(body, leak) {
				t.Errorf("%s leaks %q:\n%s", name, leak, body)
			}
		}
	}
}

// go-git 的报错不得走进响应，也不得走进日志（规范 §19、§21、§22）。
//
// 它们带着仓库路径、主机名、端口与传输细节，而日志会被转发到 Cloud
// Logging —— 与响应体一样是一条出境通道。两个方向都断言：只看响应体的话，
// 一个把原始错误照抄进日志的实现是绿的。
func TestWritebackPushErrorNeverLeaksTheTransportDetail(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	const raw = "ssh: handshake failed for git@gitlab.internal.example:2222/net/policies.git: " +
		"knownhosts key mismatch (offending line 3)"
	f.writer.err = fmt.Errorf("push: %s", raw)

	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
		map[string]any{"branch": plan.Plan.Branch, "fingerprint": plan.Plan.Fingerprint})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{"gitlab.internal.example", "knownhosts", "handshake", "2222", raw} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the response leaked %q: %s", leak, rec.Body.String())
		}
		if strings.Contains(f.logs.String(), leak) {
			t.Errorf("the log leaked %q: %s", leak, f.logs.String())
		}
	}
	// 一次失败的推送不得留下 commit：last_written_commit 是漂移检测的基准。
	if len(f.reg.writebackPushes) != 0 {
		t.Errorf("a failed push wrote %d audit rows", len(f.reg.writebackPushes))
	}
	if got := f.reg.clusters["prod-asia-1"].Git.LastWrittenCommit; got != "" {
		t.Errorf("last_written_commit = %q after a failed push", got)
	}
}

// 未绑定的集群、不存在的集群都是业务失败，且不得留下任何审计行。
func TestWritebackRefusesAnUnboundCluster(t *testing.T) {
	f := newWritebackFixture(t)

	for _, path := range []string{
		"/api/v1/clusters/prod-eu-1/policy-writeback/plan",
		"/api/v1/clusters/zz-nope/policy-writeback/plan",
	} {
		rec := authedPostJSON(t, f.h, f.cookie, path, map[string]any{})
		if got := bodyOf(t, rec)["code"]; got != float64(20002) {
			t.Errorf("%s code = %v, want 20002 (%s)", path, got, rec.Body.String())
		}
	}
	if len(f.reg.writebackPlans) != 0 {
		t.Errorf("a refused plan wrote %d audit rows", len(f.reg.writebackPlans))
	}
}

// 未配置校验器（未配置 secrets）时一律拒绝：没做过的检查不是通过了的检查。
//
// 缺省形态必须是关的 —— 这条路径的终点是生产集群的策略集合。
func TestWritebackRefusesWithoutAVerifier(t *testing.T) {
	reg := fixtureSource()
	reg.repos[gitRepoID] = boundRepo()
	c := reg.clusters["prod-asia-1"]
	c.Git = &registry.GitBinding{
		RepoID: gitRepoID, PolicyPath: writebackPolicyPath,
		VerifyResult: registry.BindingVerifyOK,
	}
	reg.clusters["prod-asia-1"] = c
	reader := store.NewFixtureReader(fixture.Load(), reg)
	writer := &fakePolicyWriter{}
	// 校验器为 nil，写入器装着：拒绝必须来自"没校验过"，不是来自"没装
	// 写入器"，否则这条用例证明不了它想证明的那件事。
	h, _, cookie := buildTestRouterWithLog(t, reader, reg, nil, writer, nil, "ERROR", io.Discard)

	rec := authedPostJSON(t, h, cookie, writebackPlanPath, map[string]any{})
	body := bodyOf(t, rec)
	if got := body["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 (%s)", got, rec.Body.String())
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "NOT_VERIFIED") {
		t.Errorf("msg = %q, want the verdict that blocked the write", msg)
	}
	if writer.calls != 0 {
		t.Errorf("an unverified binding reached the writer %d times", writer.calls)
	}
}

// 按命名空间筛选的写回必须拒绝，理由与导出同源：预测恒按整集群计算，
// 一份被裁剪的文件配上整集群口径的计数，描述的是另一套策略集。
//
// 默默忽略这个参数比拒绝更糟 —— 操作者以为自己只推了一个命名空间。
func TestWritebackRefusesANamespaceFilter(t *testing.T) {
	f := newWritebackFixture(t)

	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath+"?namespace=payment", map[string]any{})
	body := bodyOf(t, rec)
	if got := body["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 (%s)", got, rec.Body.String())
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "按整集群计算") {
		t.Errorf("msg = %q, want the reason a filtered write-back cannot carry an honest count", msg)
	}
	if len(f.reg.writebackPlans) != 0 {
		t.Errorf("a refused plan wrote %d audit rows", len(f.reg.writebackPlans))
	}
	// 不带筛选仍然出得了计划 —— 守卫没有把功能一起关掉。
	fetchPlan(t, f)
}

// 请求带回的分支必须是这个集群的一份计划所能产出的那种分支名。
//
// 分支名进指纹，因此一个不合形状的分支本来也过不了指纹那一关；但它同时
// 承载着这份计划的时刻（写回文件注释头取的就是它），所以先在这里挡掉 ——
// 一个声称自己来自明天的分支，说的是一件没有发生过的事。
func TestWritebackPushRefusesABranchItCouldNotHavePlanned(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	for _, branch := range []string{
		"main",
		"distill/prod-asia-1",
		"distill/prod-eu-1-20260814T120000Z",        // 别的集群
		"distill/prod-asia-1-29990101T000000Z",      // 未来
		"distill/prod-asia-1-20260814T120000Z/../x", // 不是这个形状
		"",
	} {
		rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath,
			map[string]any{"branch": branch, "fingerprint": plan.Plan.Fingerprint})
		if got := bodyOf(t, rec)["code"]; got != float64(20001) {
			t.Errorf("branch %q: code = %v, want 20001 (%s)", branch, got, rec.Body.String())
		}
	}
	if f.writer.calls != 0 {
		t.Fatalf("a malformed branch reached the writer %d times", f.writer.calls)
	}
}

func TestWritebackRequiresSession(t *testing.T) {
	f := newWritebackFixture(t)
	for _, path := range []string{writebackPlanPath, writebackPushPath} {
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", path, rec.Code)
		}
	}
	if f.writer.calls != 0 {
		t.Fatalf("an unauthenticated request reached the writer %d times", f.writer.calls)
	}
}

// 审计写不进去，这次写回就不发生：一次留不下痕迹的计划，事后没有任何
// 东西能回答"谁看过哪一份"。失败方向必须是不出计划，不是不写审计。
func TestWritebackPlanFailsWhenTheAuditCannotBeWritten(t *testing.T) {
	f := newWritebackFixture(t)
	f.reg.failWritesWith = fmt.Errorf("audit sink down at 10.0.0.5:3306")

	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath, map[string]any{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("the response leaked an internal address: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "distill/") {
		t.Errorf("a plan was served despite the audit failing: %s", rec.Body.String())
	}
}
