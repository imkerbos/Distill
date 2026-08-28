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
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/gitwrite"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshotstore"
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
	// ingest 让用例改「从什么时候开始观测」——学习期门禁按它判。
	ingest *stubObservedSince
	logs   *bytes.Buffer
}

// stubObservedSince 是学习期门禁要的那一点点摄入信息。
//
// span 与 covered 分开设：门禁拿覆盖判定，而两者的差额是"采集断过"这条
// 单独的提示 —— 把它们并成一个字段，那条提示就没有东西守得住。
type stubObservedSince struct {
	// observedSince 是最早一次摄入的时刻，跨度由它推出来。
	observedSince time.Time
	// covered 为零时按"连续观测到现在"处理，即覆盖等于跨度。
	covered time.Duration
}

func (s *stubObservedSince) LatestIngest(
	context.Context, string,
) (snapshotstore.IngestSummary, error) {
	return snapshotstore.IngestSummary{}, nil
}

func (s *stubObservedSince) ObservedCoverage(
	context.Context, string,
) (time.Duration, time.Duration, bool, error) {
	span := time.Since(s.observedSince)
	covered := s.covered
	if covered == 0 {
		covered = span
	}
	return span, covered, true, nil
}

// newWritebackFixture 装一个绑定了策略仓库、校验器结论为 OK 的集群。
//
// reader 与 handler 共用**同一个**注册表：写回的计数取的是应用人工决定
// 之后那一套，两边各拿一个注册表的话，两套计算恒等，「计数取自哪一套」
// 就没有东西守得住（与导出那条用例同一个理由）。
//
// 校验器结论默认 OK：仓库级不是 OK 时写回一律拒绝，而那条拒绝正是本组
// 里要能被单独打开的开关 —— 默认就关着的话，其余每条用例都停在它上面。
// realClusterPolicy 是合成数据集里 prod-asia-1 真实存在的那条 default-deny。
// 删掉它，payment 那一片就从"有规则"变回默认放行 —— 删除影响算得出来。
const realClusterPolicy = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-ingress
  namespace: payment
spec:
  podSelector: {}
  policyTypes:
  - Ingress
`

// notInThisClusterPolicy 是一份仓库里有、集群里没有的策略：从没被 apply 过，
// 或者已经被人手工删掉了。删掉这个文件对集群没有任何影响。
const notInThisClusterPolicy = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: never-applied
  namespace: payment
spec:
  podSelector: {}
  policyTypes:
  - Ingress
`

func newWritebackFixture(t *testing.T) *writebackFixture {
	t.Helper()
	return newWritebackFixtureWithReader(t, func(r store.Reader) store.Reader { return r })
}

// newWritebackFixtureWithReader 允许用例在 Reader 外面包一层。
//
// 用装饰器而不是再写一个替身 Reader：替身要自己实现整个接口，而那份实现会
// 与真实那份慢慢分家 —— 一条用例于是在测一个只存在于测试里的行为。
func newWritebackFixtureWithReader(
	t *testing.T, wrap func(store.Reader) store.Reader,
) *writebackFixture {
	t.Helper()

	reg := fixtureSource()
	reg.repos[gitRepoID] = boundRepo()
	c := reg.clusters["prod-asia-1"]
	c.Git = &registry.GitBinding{
		RepoID: gitRepoID, PolicyPath: writebackPolicyPath,
		VerifyResult: registry.BindingVerifyNotVerified,
	}
	// 登记业务周期：没有它写回一律被拒（design doc 2026-08-25 §5），
	// 而这一组用例测的是它之后的那些门禁。**"没登记就拒绝"由
	// TestWritebackRefusesAClusterWithNoDeclaredCycle 单独钉住。**
	c.BusinessCycle = 24 * time.Hour
	c.BusinessCycleReason = "这个集群的流量是日周期的"
	reg.clusters["prod-asia-1"] = c

	reader := wrap(store.NewFixtureReader(fixture.Load(), reg))
	gv := &stubGitVerifier{result: registry.BindingVerifyOK}
	// 仓库现状默认非空：策略目录下已经有一个平台不会碰的文件，仓库上还攒着
	// 一条没人合的 distill 分支。两份清单都为空的话，"计划是否真的报出了它们"
	// 根本区分不出来 —— 一个恒返回空清单的实现照样绿。
	// 仓库现状默认非空，且三个文件覆盖三种处置：一份集群里真实存在的策略
	// （可删，带影响）、一份仓库里有集群里没有的（NOT_APPLIED）、一份平台
	// 看不懂的（UNPARSEABLE，永不提供删除）。
	writer := &fakePolicyWriter{listing: gitwrite.RepoListing{
		Files: []gitwrite.RepoFile{
			{Path: writebackPolicyPath + "/README.md", Content: "# 这个目录放策略\n"},
			{Path: writebackPolicyPath + "/legacy.yaml", Content: notInThisClusterPolicy},
			{Path: writebackFilePath, Content: realClusterPolicy},
		},
		Branches: []string{"distill/prod-asia-1-20260801T090000Z"},
	}}
	var logs bytes.Buffer
	// 观测早就覆盖了下面登记的周期：这一组用例测的不是学习期门禁。
	ingest := &stubObservedSince{observedSince: time.Now().Add(-30 * 24 * time.Hour)}
	h, _, cookie := buildTestRouterWithLog(t, reader, reg, gv, writer, nil, "ERROR", &logs, ingest)

	return &writebackFixture{
		h: h, cookie: cookie, reg: reg, reader: reader,
		verifier: gv, writer: writer, ingest: ingest, logs: &logs,
	}
}

// planDeletion 是计划里一条多余文件的处置结论。
type planDeletion struct {
	Path      string         `json:"path"`
	Class     string         `json:"class"`
	Documents int            `json:"documents"`
	Counts    map[string]int `json:"counts"`
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
		Deletions  []planDeletion `json:"deletions"`
		Exclusions []struct {
			Namespace           string  `json:"namespace"`
			Workload            string  `json:"workload"`
			UnderPermissiveRate float64 `json:"underPermissiveRate"`
		} `json:"exclusions"`
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
	if reflect.DeepEqual(pv.Prediction.Counts, pv.overriddenPrediction().Counts) {
		t.Fatalf("both computations report the same counts %v — this test cannot tell them apart",
			pv.Prediction.Counts)
	}
	want := map[string]int{}
	for _, k := range predict.AllChangeKinds() {
		want[string(k)] = pv.overriddenPrediction().Counts[k]
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
	// 审计里的文件数跟着计划走。写回按命名空间切分之后它不再是 1
	// （design doc 2026-08-24 §3），但它必须等于平台真的算出来的那个数 ——
	// 断言的是"审计记的是服务端算的"，不是某个具体数字。
	if got.Files != len(plan.Plan.Files) {
		t.Errorf("audited file count = %d, want %d — the request body was believed",
			got.Files, len(plan.Plan.Files))
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
	if len(plan.Plan.Files) == 0 {
		t.Fatal("plan has no files")
	}
	// 落点必须在绑定的 policyPath 之内 —— 一份能写出 policyPath 之外的
	// 计划，等于让平台改写仓库里任意文件。**逐个查**，不只查第一个：
	// 切分之后漏检的那一个就是会被挑中的那一个。
	for _, file := range plan.Plan.Files {
		if !strings.HasPrefix(file.Path, writebackPolicyPath+"/") {
			t.Errorf("file path %q is not inside the binding's policyPath %q",
				file.Path, writebackPolicyPath)
		}
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

	// 仓库里那两个文件现在都属于「本次候选集不包含」：别人放的 legacy.yaml
	// 一直如此；旧布局留下的 distill-policy.yaml 则是切分之后的产物 ——
	// 它不会被自动删掉，而是进删除流程由人确认（design doc 2026-08-24 §3.5）。
	want := []string{
		writebackPolicyPath + "/README.md",
		writebackPolicyPath + "/distill-policy.yaml",
		writebackPolicyPath + "/legacy.yaml",
	}
	got := append([]string(nil), plan.Plan.Extraneous...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plan extraneous = %v, want %v —— 多余文件的清单不是这次枚举的结果",
			got, want)
	}
	// 平台这次要写的那些文件一个都不许出现在多余清单里：列进去，操作者会去
	// 删掉一份平台正要更新的策略。
	planned := map[string]bool{}
	for _, file := range plan.Plan.Files {
		planned[file.Path] = true
	}
	for _, p := range plan.Plan.Extraneous {
		if planned[p] {
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

	// 写回按命名空间切分之后，一份写回文件与整集群导出不再逐字节相等 ——
	// 注释头本来就不同（本文件命名空间、文档份数）。仍然必须成立的是那条
	// 反分家的性质：**策略文档来自同一段渲染**。因此比的是文档部分：
	// 把整集群导出按命名空间切开，逐个与对应的写回文件比对。
	// 写回按主体与方向切分，一个文件恰好一份文档；整集群导出是同一批文档的
	// 全集。逐份比对：写回文件里那一份，必须逐字节出现在导出里。
	exported := map[string]bool{}
	for _, docs := range docsByNamespace(t, rec.Body.String()) {
		for _, d := range docs {
			exported[d] = true
		}
	}
	if len(exported) == 0 {
		t.Fatal("export served no policy documents — this test's comparison is vacuous")
	}
	for _, file := range plan.Plan.Files {
		var got []string
		for _, docs := range docsByNamespace(t, file.Content) {
			got = append(got, docs...)
		}
		if len(got) != 1 {
			t.Errorf("file %q holds %d documents, want exactly 1 —— 一个主体一个方向一个文件",
				file.Path, len(got))
			continue
		}
		if !exported[got[0]] {
			t.Errorf("file %q 里的文档不在导出里 —— 两条路径的渲染已经分家了:\n%s",
				file.Path, got[0])
		}
	}
}

// docsByNamespace 把一份 YAML 文档流按命名空间归类，丢掉注释头。
//
// 只留文档本身：注释头里有导出者、时刻与份数，它们本来就该在两条路径上不同，
// 而"策略内容有没有分家"才是这条性质要守的东西。
func docsByNamespace(t *testing.T, content string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	parts := strings.Split(content, "\n---\n")
	for _, doc := range parts[1:] {
		doc = strings.TrimRight(doc, "\n")
		var ns string
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, "  namespace: ") {
				ns = strings.TrimSpace(strings.TrimPrefix(line, "  namespace: "))
				break
			}
		}
		out[ns] = append(out[ns], doc)
	}
	return out
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
		"命名空间筛选: ",
		"", // 主题与正文之间那个空行

		// 没观测到流量时 renderPolicyBasis 走的那一支：明说 dry-run 没做过，
		// 而不是印四个 0。取值全是常量文字。
		"dry-run: 没有做过。",
		"  「应用这些策略会拦断什么」",
		"下面的策略来自资产推导（Baseline）",
		"**但在有流量数据之前",

		// 缺口那几行（renderPolicyCaveats）。每一行的可变部分只有三类：
		// 计数、封闭枚举取值、以及集群里的对象名（命名空间/workload/
		// Service/策略名）。前两类不是内容；第三类本来就逐字写在这次提交
		// 的 YAML 里 —— 说出"payment 这个命名空间的基线推不出来"，不比
		// 提交一份 payment 的 NetworkPolicy 多暴露任何东西。
		//
		// 凭据、仓库地址与 host key 到不了这里：renderPolicyCaveats 的入参
		// 只有 store.PolicyPreview，它里面根本没有这些字段。和函数注释里
		// 那句一样，这是结构上的，不是靠渲染时记得不写。
		"—— 以下是平台知道自己没看全的地方 ——",
		"观测窗口完整度: ",
		"规则粒度: ",
		"参与推导的基线类别: ",
		"基线推导不出: ", "基线不适用: ", "基线未评估: ",
		"集群既有策略挂不上主体: ", "基线规则挂不上主体: ",
		"规则被放宽: ", "暴露放宽: ",
		"排除的命名空间: ", "排除的主体: ",
		"生成不出规则的流: ", "失效的人工决定: ",
		"除以上两项外，本次没有其他缺口",
		"  - ", // 上面各项的逐条列举；形状由下面那条正向断言另行约束
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
	// "  - " 是一条前缀通配，它一个人就能让任何东西过关——所以配一条正向
	// 断言把那个洞补上：提交信息里一个 IP、一个 URL scheme 都不许出现。
	//
	// 这不是多余的：缺口那几行会把集群里的对象名插进这段文字，而"对象名"
	// 与"内部地址"之间只隔着上游哪天往某个 Detail 字段里塞了个 endpoint。
	// 逐条列举里带上自由文本正是这一版删掉的东西，这条断言是它的锁。
	addrLike := regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b|[a-z][a-z0-9+.-]*://`)
	if m := addrLike.FindString(plan.Plan.CommitMessage); m != "" {
		t.Errorf("提交信息里出现了看起来像地址的 %q —— 这段文字会落进仓库历史，撤不回来:\n%s",
			m, plan.Plan.CommitMessage)
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

// 策略按 namespace 切分成多个文件（design doc 2026-08-24 §3）。
//
// 单文件装下整集群会让评审粒度错位：改一个 workload 的一条端口，diff 落在
// 一份几百行的文件上，评审人无法只看自己那一块，多人并行确认不同命名空间
// 时必然冲突。切分之后 CODEOWNERS 也才有边界可分。
func TestWritebackSplitsPoliciesPerNamespace(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	if len(plan.Plan.Files) < 2 {
		t.Fatalf("plan wrote %d file(s); 这份候选集跨多个命名空间，应当切分",
			len(plan.Plan.Files))
	}

	seen := map[string]bool{}
	for _, file := range plan.Plan.Files {
		if !strings.HasPrefix(file.Path, writebackPolicyPath+"/") {
			t.Errorf("file %q 落在 policyPath 之外", file.Path)
			continue
		}
		// 落点是 <policyPath>/<namespace>/distill-policy.yaml：一个命名空间
		// 一个目录，而不是一个 <namespace>.yaml —— 后者会让平台静默覆盖
		// 仓库里别人放的同名文件（design doc 2026-08-24 §3.1）。
		// 落点是 <policyPath>/distill/<namespace>.yaml。文件名必须是命名空间：
		// 每个目录里都叫同一个名字，会让一次写回在 PR 的文件列表里显示成
		// 十几行同名条目，评审人分辨不出该先看哪一块（design doc §3.1）。
		// 落点是 distill/<namespace>/<workload>-<方向>.yaml：命名空间分目录，
		// 文件名点名主体与方向（design doc 2026-08-24 §3.1、§3.6）。
		rest := strings.TrimPrefix(file.Path, writebackPolicyPath+"/distill/")
		ns, base, ok := strings.Cut(rest, "/")
		if rest == file.Path || !ok || !strings.HasSuffix(base, ".yaml") {
			t.Errorf("file %q 不是 distill/<namespace>/<workload>-<方向>.yaml 形状", file.Path)
			continue
		}
		if !strings.HasSuffix(base, "-ingress.yaml") && !strings.HasSuffix(base, "-egress.yaml") {
			t.Errorf("file %q 的文件名没有点明方向", file.Path)
		}
		// 一个命名空间现在有多个文件（每个主体两份），因此这里记的是
		// 「路径不重复」，而重复路径本身由 registry.NewWritebackPlan 拒掉。
		if seen[file.Path] {
			t.Errorf("同一条路径 %q 出现了两次", file.Path)
		}
		seen[file.Path] = true

		// 一个文件里只允许出现它自己那个命名空间的策略。混进别的命名空间，
		// 切分就没有意义 —— 评审人仍然要读整份才知道影响到谁。
		for _, got := range namespacesIn(t, file.Content) {
			if got != ns {
				t.Errorf("file %q 里出现了命名空间 %q 的策略", file.Path, got)
			}
		}
	}
}

// 每个文件的注释头里的四类计数是**整集群**那一份（design doc 2026-08-24 §3.4）。
//
// 按命名空间拆计数会漏掉跨命名空间的影响，而那正是 NetworkPolicy 最容易
// 出事的地方。既然给的是整集群口径，文件里就必须说出这一点，否则读者会把
// 它当成"本命名空间的影响"。
func TestEachWritebackFileCarriesTheClusterWideCounts(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	for _, file := range plan.Plan.Files {
		head := file.Content
		if i := strings.Index(head, "\n---\n"); i >= 0 {
			head = head[:i]
		}
		for _, k := range predict.AllChangeKinds() {
			want := fmt.Sprintf("dry-run %s: %d", k, plan.Plan.Counts[string(k)])
			if !strings.Contains(head, want) {
				t.Errorf("file %q 的注释头缺少整集群计数 %q", file.Path, want)
			}
		}
		if !strings.Contains(head, "整集群") {
			t.Errorf("file %q 没有说明这几个数字是整集群口径", file.Path)
		}
	}
}

// namespacesIn 取出一份 YAML 文档流里每份策略的命名空间。
//
// 用文本解析而不是反序列化成类型：断言的是**真的写进仓库的那份文本**，
// 拿被测代码的类型去解自己的输出，一个渲染缺陷可以两边同时存在而测试全绿。
func namespacesIn(t *testing.T, content string) []string {
	t.Helper()
	var out []string
	for _, doc := range strings.Split(content, "\n---\n") {
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, "  namespace: ") {
				out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "  namespace: ")))
				break
			}
		}
	}
	return out
}

// 多余文件必须被分类，且可删的那一类要带删除影响（design doc 2026-08-24 §4）。
//
// 在此之前删除是这个平台唯一一类不被预测的变更。一份只说"这几个文件是多余的"
// 的清单，操作者要么不敢动、要么凭感觉删 —— 而删掉一条 default-deny，
// 那一片就从有规则变回默认放行。
func TestPlanClassifiesExtraneousFilesAndPredictsDeletion(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	byPath := map[string]planDeletion{}
	for _, d := range plan.Plan.Deletions {
		byPath[d.Path] = d
	}

	// 集群里真实存在的那条策略：可删，且必须带四类计数。
	real := byPath[writebackPolicyPath+"/distill-policy.yaml"]
	if real.Class != "DELETABLE" {
		t.Errorf("集群里真实存在的策略文件被分类成 %q，want DELETABLE", real.Class)
	}
	if len(real.Counts) == 0 {
		t.Error("可删的文件没有带删除影响 —— 那就还是一次没被评估的变更")
	}

	// 仓库里有、集群里没有的：删掉它对集群没有影响，但仍然要人确认。
	notApplied := byPath[writebackPolicyPath+"/legacy.yaml"]
	if notApplied.Class != "NOT_APPLIED" {
		t.Errorf("仓库里有集群里没有的文件被分类成 %q，want NOT_APPLIED", notApplied.Class)
	}

	// 平台看不懂的：永不提供删除。
	unparseable := byPath[writebackPolicyPath+"/README.md"]
	if unparseable.Class != "UNPARSEABLE" {
		t.Errorf("平台解析不了的文件被分类成 %q，want UNPARSEABLE", unparseable.Class)
	}
	if len(unparseable.Counts) != 0 {
		t.Error("平台看不懂的文件带上了删除影响 —— 它算不出那个数字")
	}
}

// 确认删除一个平台没有提供删除入口的文件，一律拒绝。
//
// 这是「请求体不是授权」那条纪律在删除上的形态：路径由请求带来，能不能删
// 由平台这一刻重算出的清单说了算。
func TestPushRefusesToDeleteAFileThePlanNeverOffered(t *testing.T) {
	f := newWritebackFixture(t)
	plan := fetchPlan(t, f)

	rec := authedPostJSON(t, f.h, f.cookie, writebackPushPath, map[string]any{
		"branch":      plan.Plan.Branch,
		"fingerprint": plan.Plan.Fingerprint,
		"deletions":   []string{writebackPolicyPath + "/README.md"},
	})
	if got := bodyOf(t, rec)["code"]; got == float64(0) {
		t.Fatalf("平台接受了一次它没有提供的删除：%s", rec.Body.String())
	}
	if f.writer.calls != 0 {
		t.Errorf("被拒的推送仍然到达了写入器 %d 次", f.writer.calls)
	}
}

// 确认的删除必须逐条出现在提交信息里（design doc 2026-08-24 §4.5）。
//
// 评审人在合并请求上唯一会读的就是这段文字。删除只体现在 diff 里，等于让
// 一次"把某一片策略从集群里撤掉"的变更在他读到的那段话里完全不存在。
func TestCommitMessageListsTheConfirmedDeletions(t *testing.T) {
	f := newWritebackFixture(t)
	target := writebackPolicyPath + "/legacy.yaml"

	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath, map[string]any{
		"deletions": []string{target},
	})
	body := bodyOf(t, rec)
	if got := body["code"]; got != float64(0) {
		t.Fatalf("plan code = %v (%s)", got, rec.Body.String())
	}
	var env struct {
		Data planView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	view := env.Data

	if !strings.Contains(view.Plan.CommitMessage, target) {
		t.Errorf("提交信息里没有点名被删掉的 %q：\n%s", target, view.Plan.CommitMessage)
	}
	if !strings.Contains(view.Plan.CommitMessage, "删除") {
		t.Errorf("提交信息里没有说这次删了东西：\n%s", view.Plan.CommitMessage)
	}
}

// lateApplyReader 装成「这些策略现在在集群里，但观测窗口那一刻还不在」。
//
// 这正是 GitOps 下的常态时序：平台在窗口 W 出计划，人合并，controller 在 W
// 之后把策略下发进集群。下一次出计划时，那批对象是活的，但窗口锚点的快照里
// 还没有它们。
type lateApplyReader struct {
	store.Reader
}

func (r lateApplyReader) DeletionImpact(
	ctx context.Context, clusterID string, window store.TimeWindow,
	removed []networkingv1.NetworkPolicy,
) (store.DeletionImpactReport, error) {
	base, err := r.Reader.DeletionImpact(ctx, clusterID, window, removed)
	if err != nil {
		return base, err
	}
	// 现在在集群里（Live），但窗口那一刻不在（InWindow = 0）。
	base.Live = len(removed)
	base.InWindow = 0
	return base, nil
}

// 一条在观测窗口之后才被下发的策略，不得被标成「删掉没影响」
// （design doc 2026-08-24 §4.2，2026-08-24 实测修订）。
//
// 这是这条路径唯一那个朝**让人放心**的方向失败的形态：重放算出来的删除影响
// 是"删掉一个当时并不存在的东西"，结果恒为无变化。操作者读到一份写着
// "NOT_APPLIED / 删除对集群没有影响"的清单，勾掉它，而那条策略正在生效。
func TestAPolicyAppliedAfterTheWindowIsNotCalledHarmless(t *testing.T) {
	f := newWritebackFixtureWithReader(t, func(r store.Reader) store.Reader {
		return lateApplyReader{Reader: r}
	})
	plan := fetchPlan(t, f)

	if len(plan.Plan.Deletions) == 0 {
		t.Fatal("计划里没有任何多余文件 —— 这条用例没有被测对象")
	}
	for _, d := range plan.Plan.Deletions {
		switch d.Class {
		case "NOT_APPLIED":
			t.Errorf("%s 被标成「集群里没有、删掉无影响」，而它此刻正在集群里生效", d.Path)
		case "DELETABLE":
			t.Errorf("%s 被标成可删并给出了删除影响 %v，而那份计数算的是"+
				"删掉一个窗口内并不存在的东西", d.Path, d.Counts)
		case "IMPACT_UNKNOWN":
			if len(d.Counts) != 0 {
				t.Errorf("%s 算不出影响却带上了计数 %v", d.Path, d.Counts)
			}
		}
	}
}

// occupiedNameReader 装成「集群里已经有一个与候选集同名的对象」。
type occupiedNameReader struct {
	store.Reader
}

func (r occupiedNameReader) LivePolicies(
	_ context.Context, _ string,
) ([]networkingv1.NetworkPolicy, error) {
	// 平台会生成 candidate-<workload>-ingress。这里声称集群里已经有一个，
	// 而仓库的 distill/ 子树里没有任何文件声明过它 —— 也就是说它不是
	// 平台这条链路放进去的。
	return []networkingv1.NetworkPolicy{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "candidate-api-ingress"},
	}}, nil
}

// 集群里已存在同名对象、且仓库里没有任何文件声明过它时，写回必须**拒绝出计划**
// （design doc 2026-08-25 §4）。
//
// `candidate-` 只是命名约定，不是保留字。覆盖别人的策略对象，症状是那一片的
// 放行规则被悄悄换掉，而 Git 历史里只看得到平台写了一个文件 —— 没有任何一屏
// 显示"你刚才顶掉了谁的东西"。
func TestWritebackRefusesToOverwriteAnObjectItDoesNotOwn(t *testing.T) {
	f := newWritebackFixtureWithReader(t, func(r store.Reader) store.Reader {
		return occupiedNameReader{Reader: r}
	})

	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath, map[string]any{})
	body := bodyOf(t, rec)
	if got := body["code"]; got == float64(0) {
		t.Fatalf("平台照常出了计划，而集群里那个对象不是它的：%s", rec.Body.String())
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "candidate-api-ingress") {
		t.Errorf("拒绝理由没有点名冲突的对象：%q", msg)
	}
	if f.writer.calls != 0 {
		t.Errorf("被拒的计划仍然到达了写入器 %d 次", f.writer.calls)
	}
}

// 对照组：同名对象**已经被仓库里的文件声明过**时不算冲突 —— 那是平台自己
// 上一轮写出去、由 GitOps 落下来的东西，正常更新它。
//
// 没有这一条，一个"凡是集群里有同名对象就拒"的实现照样能让上面那条通过，
// 而那会让第二次写回永远出不了计划。
func TestWritebackUpdatesItsOwnObjectsWithoutComplaining(t *testing.T) {
	f := newWritebackFixtureWithReader(t, func(r store.Reader) store.Reader {
		return ownedNameReader{Reader: r}
	})
	plan := fetchPlan(t, f)
	if len(plan.Plan.Files) == 0 {
		t.Fatal("平台把自己上一轮写出去的对象当成了冲突")
	}
}

// ownedNameReader 声称集群里有一个对象，而它正是仓库现有文件声明的那一份。
type ownedNameReader struct {
	store.Reader
}

func (r ownedNameReader) LivePolicies(
	_ context.Context, _ string,
) ([]networkingv1.NetworkPolicy, error) {
	// realClusterPolicy 就是 fixture 里那个仓库文件的内容（payment/default-deny-ingress）。
	return []networkingv1.NetworkPolicy{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payment", Name: "default-deny-ingress"},
	}}, nil
}

// disagreeingReader 装成「payment/api 这个 workload 上，平台判定与执行平面
// 大比例对不上，且分歧全部落在会造成阻断的那个方向」。
type disagreeingReader struct {
	store.Reader
}

func (r disagreeingReader) Reconciliation(
	_ context.Context, clusterID string, window store.TimeWindow,
) (store.ReconciliationReport, error) {
	subject := reconcile.Subject{Namespace: "payment", Workload: "api"}
	return store.ReconciliationReport{
		Cluster: clusterID, Window: window, SourceReportsVerdicts: true,
		Report: reconcile.Report{
			Total:   10,
			Overall: reconcile.Counts{reconcile.ClassAgree: 8, reconcile.ClassUnderPermissive: 2},
			BySubject: []reconcile.SubjectCounts{{
				Subject: subject,
				Counts: reconcile.Counts{
					reconcile.ClassAgree: 8, reconcile.ClassUnderPermissive: 2,
				},
			}},
		},
	}, nil
}

// 一个「平台低估放行面」比例超阈的 workload，它的候选规则不得被推出去
// （design doc 2026-08-25 §3.4）。
//
// 这一类分歧的含义是：平台以为这条连接现在就不通，于是不会为它生成放行规则；
// 而集群里它通着。候选集下发后它从通变成不通 —— 而 dry-run 会把它算成
// UNCHANGED，因为在平台的世界里它本来就不通。**这是唯一一条能绕过 dry-run
// 造成生产阻断的路径**，因此它必须在写回之前被拦下，而不是事后在看板上被看见。
func TestWritebackExcludesAWorkloadThePlatformDisagreesAbout(t *testing.T) {
	f := newWritebackFixtureWithReader(t, func(r store.Reader) store.Reader {
		return disagreeingReader{Reader: r}
	})

	plan := fetchPlan(t, f)

	// 被排除的那一个不进文件清单。
	for _, file := range plan.Plan.Files {
		if strings.Contains(file.Path, "/payment/api-") {
			t.Errorf("payment/api 上两成判定与集群实际执行相反，它的策略仍然被写了出去：%s",
				file.Path)
		}
	}
	// **其余照常写。** 整个集群一起卡住的话，这个平台在任何带第二策略平面
	// 的集群上都永远出不了计划 —— 而那正是它要服务的集群，运营上会逼人去
	// 调阈值绕过门禁。
	if len(plan.Plan.Files) == 0 {
		t.Fatal("一个 workload 的分歧把整个集群的写回都卡住了")
	}

	// 排除必须点名，并给出具体分歧率：0.06 与 0.9 都超阈，但前者值得去看
	// 明细、后者说明平台在这个主体上基本不成立。
	if len(plan.Plan.Exclusions) != 1 {
		t.Fatalf("Exclusions = %+v, want 恰好一条", plan.Plan.Exclusions)
	}
	ex := plan.Plan.Exclusions[0]
	if ex.Namespace != "payment" || ex.Workload != "api" {
		t.Errorf("被排除的主体 = %s/%s, want payment/api", ex.Namespace, ex.Workload)
	}
	if ex.UnderPermissiveRate != 0.2 {
		t.Errorf("UnderPermissiveRate = %v, want 0.2 —— 报的必须是真实分歧率，"+
			"不是一个布尔或一个阈值", ex.UnderPermissiveRate)
	}

	// **提交信息里必须写明。** 一份少了一个 workload 的策略集，不说明的话
	// 评审人读到的就是"这个集群只有这些 workload"—— 而缺席恰恰意味着平台在
	// 那个主体上看不准，是他最该知道的一件事。
	for _, want := range []string{"payment/api", "排除"} {
		if !strings.Contains(plan.Plan.CommitMessage, want) {
			t.Errorf("提交信息里没有 %q：\n%s", want, plan.Plan.CommitMessage)
		}
	}
}

// 被排除主体的既有策略文件不得被当成"多余"，更不得被判成可删。
//
// 分歧说明的是"平台在这个主体上看不准"，那时最不该做的就是替它做减法 ——
// 一次分歧最终把一份已经生效的策略删掉，方向完全反了。
func TestExcludedSubjectsExistingFilesAreNotDeletable(t *testing.T) {
	f := newWritebackFixtureWithReader(t, func(r store.Reader) store.Reader {
		return disagreeingReader{Reader: r}
	})
	// 仓库里已经有平台上一轮为 payment/api 写下的两个文件。
	held := []string{
		"clusters/prod-asia-1/distill/payment/api-ingress.yaml",
		"clusters/prod-asia-1/distill/payment/api-egress.yaml",
	}
	for _, p := range held {
		f.writer.listing.Files = append(f.writer.listing.Files, gitwrite.RepoFile{
			Path: p,
			Content: "apiVersion: networking.k8s.io/v1\n" +
				"kind: NetworkPolicy\nmetadata:\n  name: candidate-api-ingress\n" +
				"  namespace: payment\nspec:\n  podSelector:\n    matchLabels:\n" +
				"      app: api\n  policyTypes:\n  - Ingress\n",
		})
	}

	plan := fetchPlan(t, f)
	for _, p := range held {
		if slices.Contains(plan.Plan.Extraneous, p) {
			t.Errorf("被排除主体的既有文件 %s 被列成了多余文件", p)
		}
		for _, del := range plan.Plan.Deletions {
			if del.Path == p {
				t.Errorf("被排除主体的既有文件 %s 进了删除清单，处置为 %s", p, del.Class)
			}
		}
	}
}

// 对照组：来源根本不报判定时（conntrack 接入、或合成数据集），门禁不得拦。
//
// 没有这一条，一个"凡是对不上就拦"的实现会把所有 NODE_CONNTRACK 接入的集群
// 整个锁死 —— 而它们的一致率不是低，是**不存在**。
func TestWritebackIsNotBlockedWhenTheSourceReportsNoVerdicts(t *testing.T) {
	f := newWritebackFixture(t) // fixture 来源恒不报判定
	plan := fetchPlan(t, f)
	if len(plan.Plan.Files) == 0 {
		t.Fatal("来源不报判定时写回被拦了 —— 那不是低一致率，是没有一致率")
	}
}

// 观测还没覆盖一个完整业务周期时，写回计划拒绝出
// （design doc 2026-08-25 §5）。
//
// 候选集只学窗口内观测到的流量。一个月跑一次的批处理、只在故障时走的灾备
// 链路，不在观测里就没有规则，**且不会出现在 dry-run 的 WOULD_BREAK 里** ——
// dry-run 只能评估它见过的连接。这道门禁不消除那条根本限制，它把风险与一个
// 显式的、有人签字的判断绑在一起。
func TestWritebackWaitsForAFullBusinessCycle(t *testing.T) {
	f := newWritebackFixture(t)
	// 集群声明业务周期是 7 天，而平台只观测了 2 小时。
	c := f.reg.clusters["prod-asia-1"]
	c.BusinessCycle = 7 * 24 * time.Hour
	c.BusinessCycleReason = "月结批处理在每月最后一天跑，周级窗口能覆盖日常"
	f.reg.clusters["prod-asia-1"] = c
	f.ingest.observedSince = time.Now().Add(-2 * time.Hour)

	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath, map[string]any{})
	body := bodyOf(t, rec)
	if got := body["code"]; got == float64(0) {
		t.Fatalf("平台照常出了计划，而观测只覆盖了声明周期的很小一部分：%s", rec.Body.String())
	}
	msg, _ := body["msg"].(string)
	// 拒绝理由必须说清**还差多久** —— 一句"观测不足"会让操作者不知道该等
	// 还是该改登记。
	// 时长要写成人读得懂的样子：七天不该显示成 168h0m0s，那会让读者
	// 放弃核对，而这条文案的全部意义就是让他核对"还差多久"。
	for _, want := range []string{"7 天", "业务周期", "还差"} {
		if !strings.Contains(msg, want) {
			t.Errorf("拒绝理由里没有 %q：%q", want, msg)
		}
	}
}

// 采集断过的集群，跨度够也不放行 —— 且理由要点名"断过"，不是"再等等"。
//
// 一个集群 90 天前摄入过一次、之后采集器坏了、今天恢复：首末跨度 90 天，
// 真正收到流量的只有两小时。拿跨度与业务周期比会把它放行，而它恰恰是最不该
// 放行的那一类 —— 一份基于两小时观测的 default-deny，下发的是"这两小时之外
// 的一切都拦掉"。
//
// 文案必须把这两件事分开：光说"还差 6 天"会让操作者去等，而等下去并不会补回
// 中间那 89 天 —— 要修的是采集链路。
func TestWritebackRefusesWhenIngestionHasGaps(t *testing.T) {
	f := newWritebackFixture(t)
	c := f.reg.clusters["prod-asia-1"]
	c.BusinessCycle = 7 * 24 * time.Hour
	c.BusinessCycleReason = "月结批处理在每月最后一天跑，周级窗口能覆盖日常"
	f.reg.clusters["prod-asia-1"] = c
	// 跨度 90 天，真正观测到的只有 2 小时。
	f.ingest.observedSince = time.Now().Add(-90 * 24 * time.Hour)
	f.ingest.covered = 2 * time.Hour

	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath, map[string]any{})
	body := bodyOf(t, rec)
	if got := body["code"]; got == float64(0) {
		t.Fatalf("跨度够而观测只有两小时，平台照常出了计划：%s", rec.Body.String())
	}
	msg, _ := body["msg"].(string)
	for _, want := range []string{"2 小时", "90 天", "采集链路"} {
		if !strings.Contains(msg, want) {
			t.Errorf("拒绝理由里没有 %q，操作者会以为再等等就好：%q", want, msg)
		}
	}
}

// 观测已经覆盖一个完整周期时放行。
//
// 没有这一条，一个恒拒绝的实现照样能让上面那条通过，而那等于把写回整个关掉。
func TestWritebackProceedsOnceTheCycleIsCovered(t *testing.T) {
	f := newWritebackFixture(t)
	c := f.reg.clusters["prod-asia-1"]
	c.BusinessCycle = time.Hour
	c.BusinessCycleReason = "这个集群的流量是分钟级周期性的"
	f.reg.clusters["prod-asia-1"] = c
	f.ingest.observedSince = time.Now().Add(-8 * time.Hour)

	plan := fetchPlan(t, f)
	if len(plan.Plan.Files) == 0 {
		t.Fatal("观测已覆盖声明的周期，写回却被拦了")
	}
}

// **没有登记业务周期的集群，同样拒绝出计划。**
//
// 「不知道这个集群多久能看全一轮」比「知道它是 7 天」更危险：前者连"要不要
// 等"这个问题都没有人回答过。默认放行会让这道门禁在最需要它的集群上不存在。
func TestWritebackRefusesAClusterWithNoDeclaredCycle(t *testing.T) {
	f := newWritebackFixture(t)
	// 把 fixture 默认登记的那份清掉 —— 本条用例要的正是"没有人回答过
	// 这个问题"的形态。
	c := f.reg.clusters["prod-asia-1"]
	c.BusinessCycle = 0
	c.BusinessCycleReason = ""
	f.reg.clusters["prod-asia-1"] = c

	rec := authedPostJSON(t, f.h, f.cookie, writebackPlanPath, map[string]any{})
	body := bodyOf(t, rec)
	if got := body["code"]; got == float64(0) {
		t.Fatal("没有登记业务周期的集群照常出了计划")
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "业务周期") {
		t.Errorf("拒绝理由没有点名要登记什么：%q", msg)
	}
}
