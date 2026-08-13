package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// stubGitVerifier 是 httpapi.GitVerifier 的替身。
//
// 它记录两层各自的调用次数与看到的参数：一条「结论正确」的断言并不能证明
// 校验器真的被调用过 —— 一个把结论写死的 handler 也能让那条断言通过。两个
// 方向都要有人守。
//
// 两层分别计数而不是共用一个计数器：路径级以仓库级为前提，「先取仓库级
// 结论再做路径级」这条顺序只有在能分别数出来时才断言得了。
type stubGitVerifier struct {
	// repoResult 是 VerifyRepo 的返回值；零值时按 OK 处理，让绝大多数
	// 只关心路径级的用例不必逐个填它。
	repoResult registry.RepoVerifyResult
	// result 是 VerifyPath 的返回值。
	result registry.BindingVerifyResult
	// repoCalls 与 calls 分别是两层被调用的次数。
	repoCalls int
	calls     int
	// seenRepo 是两层最近一次看到的仓库；seenRepoResult 与 seenPath 是
	// 路径级那次调用拿到的仓库级结论与策略路径。
	seenRepo       registry.GitRepo
	seenRepoResult registry.RepoVerifyResult
	seenPath       string
	// trace 与 memRegistry 共用一份调用序列，用于确认校验发生在落库之前。
	trace *[]string
}

func (s *stubGitVerifier) VerifyRepo(
	_ context.Context, r registry.GitRepo,
) (registry.RepoVerifyResult, *time.Time) {
	s.repoCalls++
	s.seenRepo = r
	if s.trace != nil {
		*s.trace = append(*s.trace, "verify-repo")
	}
	result := s.repoResult
	if result == "" {
		result = registry.RepoVerifyOK
	}
	// 结论未登记时不给时刻：真实实现里那种取值来自底层错误文本，而
	// 「校验发生过」这件事只有在结论可用时才谈得上。
	if !result.Valid() {
		return result, nil
	}
	now := time.Now().UTC()
	return result, &now
}

func (s *stubGitVerifier) VerifyPath(
	_ context.Context, r registry.GitRepo,
	repoResult registry.RepoVerifyResult, policyPath string,
) (registry.BindingVerifyResult, *time.Time) {
	s.calls++
	s.seenRepo = r
	s.seenRepoResult = repoResult
	s.seenPath = policyPath
	if s.trace != nil {
		*s.trace = append(*s.trace, "verify")
	}
	// 与 gitverify.Verifier.VerifyPath 一样短路：仓库级不是 OK 时路径级
	// 是 NOT_VERIFIED 且没有时刻。替身若不带这条，「仓库级不 OK 时路径级
	// 不得报 PATH_MISSING」这条约束在 handler 测试里就没有东西守得住。
	if repoResult != registry.RepoVerifyOK {
		return registry.BindingVerifyNotVerified, nil
	}
	if !s.result.Valid() {
		return s.result, nil
	}
	now := time.Now().UTC()
	return s.result, &now
}

// gitRepoID 是测试仓库的标识。
const gitRepoID = "policies"

// boundRepo 是被 boundCluster 绑定的那个仓库。
func boundRepo() registry.GitRepo {
	return registry.GitRepo{
		ID: gitRepoID, URL: "ssh://git@gitlab.example.com/net/policies.git",
		Branch: "main", CredentialRef: gitBindingRef,
		VerifyResult: registry.RepoVerifyNotVerified,
	}
}

// boundCluster 是一个带 Git 绑定的集群，供重校验用例复用。
//
// VerifyResult 写成 NOT_VERIFIED 而不是留零值：库里的每一行都带着一个
// 已登记的取值（BindGitRepo 会把空值落成 NOT_VERIFIED），一个空串的
// fixture 描述的是一种存不进库的状态。
func boundCluster() registry.Cluster {
	return registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
		Git: &registry.GitBinding{
			RepoID: gitRepoID, PolicyPath: "clusters/c1",
			VerifyResult: registry.BindingVerifyNotVerified,
		},
	}
}

// boundRegistry 是一个装着 boundCluster 与它所绑仓库的注册表替身。
//
// 绑定端点的每一条用例都需要仓库真的在库里：绑定指向一个不存在的仓库时
// handler 答「不存在」，那会让下面的断言全部落在一条根本没走到校验的
// 路径上。
func boundRegistry() *memRegistry {
	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	reg.repos[gitRepoID] = boundRepo()
	return reg
}

func authedPostNoBody(t *testing.T, h http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const verifyPath = "/api/v1/clusters/c1/git-binding/verify"

func TestVerifyGitBindingRequiresSession(t *testing.T) {
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), boundRegistry())

	req := httptest.NewRequest(http.MethodPost, verifyPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// 重校验的结论必须同时进响应与库：只回不落库，界面刷新一次就退回旧结论；
// 只落库不回，操作者点完看不到任何变化。
func TestVerifyGitBindingStoresAndReturnsTheVerdict(t *testing.T) {
	reg := boundRegistry()
	stub := &stubGitVerifier{result: registry.BindingVerifyPathMissing}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, verifyPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", body["code"], rec.Body.String())
	}

	// 校验器确实被调用过，且拿到的是库里那个绑定 —— 否则一个把结论写死
	// 的 handler 也能让下面的断言全绿。
	if stub.calls != 1 {
		t.Fatalf("verifier calls = %d, want exactly 1", stub.calls)
	}
	if stub.repoCalls != 1 {
		t.Fatalf("repo-level calls = %d, want exactly 1 — the path-level verdict rests on a repo-level one", stub.repoCalls)
	}
	if stub.seenRepo != boundRepo() || stub.seenPath != boundCluster().Git.PolicyPath {
		t.Errorf("verifier saw repo %+v and path %q, want the stored repo and policy path", stub.seenRepo, stub.seenPath)
	}
	// 路径级拿到的必须是**这次**跑出来的仓库级结论，不是一个凑手的常量，
	// 也不是库里那条几天前的旧结论（库里存的是 NOT_VERIFIED）。
	if stub.seenRepoResult != registry.RepoVerifyOK {
		t.Errorf("path-level got repoResult = %q, want the verdict this request derived, not a convenient one",
			stub.seenRepoResult)
	}

	data, _ := body["data"].(map[string]any)
	if data["verifyResult"] != string(registry.BindingVerifyPathMissing) {
		t.Errorf("verifyResult = %v, want PATH_MISSING", data["verifyResult"])
	}
	if data["verifiedAt"] == nil {
		t.Error("verifiedAt is absent — a check did happen, and its time is what makes the verdict readable")
	}

	stored := reg.clusters["c1"].Git
	if stored == nil || stored.VerifyResult != registry.BindingVerifyPathMissing {
		t.Fatalf("stored verifyResult = %+v, want PATH_MISSING", stored)
	}
	if stored.VerifiedAt == nil {
		t.Error("stored verifiedAt is nil, want the time of this check")
	}
}

// 未配置校验器时结论是 NOT_VERIFIED，绝不是 OK：没做过的检查不是通过了
// 的检查。这条守的是「缺省即安全」的方向 —— 一个把 nil 当成"没什么可
// 担心的"的实现，会让整个未配置 secrets 的部署显示成全绿。
//
// 一次没有发生的校验也不该留下任何写入：SetGitVerifyResult 只接受一个
// 具体的时刻，落库就等于宣称某时某刻校验过一次，并留下一条描述空白事件
// 的 VERIFY_GIT_BINDING 审计行。trace 为空是这句话唯一能被断言的形态。
func TestVerifyGitBindingWithoutAVerifierIsNotVerified(t *testing.T) {
	var trace []string
	reg := boundRegistry()
	reg.trace = &trace
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostNoBody(t, h, cookie, verifyPath)
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.BindingVerifyNotVerified) {
		t.Errorf("verifyResult = %v, want NOT_VERIFIED", data["verifyResult"])
	}
	if data["verifiedAt"] != nil {
		t.Errorf("verifiedAt = %v, want absent — no check happened, so there is no such moment", data["verifiedAt"])
	}
	if got := reg.clusters["c1"].Git.VerifyResult; got != registry.BindingVerifyNotVerified {
		t.Errorf("stored verifyResult = %q, want NOT_VERIFIED", got)
	}
	if len(trace) != 0 {
		t.Errorf("store writes = %v, want none — nothing was verified, so there is nothing to record", trace)
	}
}

// 集群不存在与集群没有绑定都是业务失败：没有可校验的对象不是服务出了问题。
func TestVerifyGitBindingWithoutATargetIsBusinessNotFound(t *testing.T) {
	reg := boundRegistry()
	unbound := boundCluster()
	unbound.Git = nil
	reg.clusters["c1"] = unbound
	stub := &stubGitVerifier{result: registry.BindingVerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	for _, path := range []string{verifyPath, "/api/v1/clusters/no-such/git-binding/verify"} {
		rec := authedPostNoBody(t, h, cookie, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 — a missing target is not a server fault", path, rec.Code)
		}
		if got := bodyOf(t, rec)["code"]; got != float64(20002) {
			t.Errorf("%s: code = %v, want 20002", path, got)
		}
	}
	// 没有绑定就不该有出站：一次凭空的 SSH 握手既没有目标，也会在日志里
	// 留下一条没人能解释的连接。
	if stub.calls != 0 || stub.repoCalls != 0 {
		t.Errorf("verifier calls = %d repo / %d path, want 0 when there is nothing to verify",
			stub.repoCalls, stub.calls)
	}
}

// registry 内部故障走真实的 500，且错误细节一个字都不能进响应体 ——
// 与 TestCreateClusterFailurePropagatesRegistryInternalError 同一条纪律，
// 这里覆盖新端点。
//
// 读与写两条分支都要打：这个 handler 先读后写，只让整个替身失败的话，
// 请求会停在读那一步，写路径上的错误处理根本没被执行 —— 而一条断言
// 在没被执行到的分支上永远成立，那正是"不可能失败的测试"的样子。
func TestVerifyGitBindingDoesNotLeakRegistryErrorText(t *testing.T) {
	const dsnish = "mysql: dial tcp 10.0.0.5:3306: connection refused"

	for _, tc := range []struct {
		name string
		fail func(*memRegistry)
	}{
		{"read", func(m *memRegistry) { m.failWith = errors.New(dsnish) }},
		{"write", func(m *memRegistry) { m.failWritesWith = errors.New(dsnish) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := boundRegistry()
			tc.fail(reg)
			stub := &stubGitVerifier{result: registry.BindingVerifyOK}
			h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

			rec := authedPostNoBody(t, h, cookie, verifyPath)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
			}
			if got := bodyOf(t, rec)["code"]; got != float64(50001) {
				t.Errorf("code = %v, want 50001", got)
			}
			if got := bodyOf(t, rec)["msg"]; got != response.CodeInternal.Message() {
				t.Errorf("msg = %q, want the fixed internal-error message %q", got, response.CodeInternal.Message())
			}
			for _, secret := range []string{"mysql", "10.0.0.5", "3306"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("response leaked %q: %s", secret, rec.Body.String())
				}
			}
		})
	}
}

// 结论字段不得成为自由文本的载体。
//
// GitVerifier 是一个接口，任何实现都可能返回一个未登记的取值 —— 而那个
// 值在这条链路上最可能的来源，正是某个底层报错的文本（go-git、SSH、
// 凭据解析）。它会同时写进 verify_result 列与响应体，于是「边界不透传
// 底层错误」这句话就只剩下 gitverify 一个包在兜。边界必须自己是结构化的：
// 不认识的值收窄成 NOT_VERIFIED，方向朝「未确认」关，不朝「可信」开。
func TestVerifyGitBindingNeverPutsFreeTextOnTheWire(t *testing.T) {
	const leaked = "ssh: handshake failed for git@gitlab.example.com:22"

	reg := boundRegistry()
	stub := &stubGitVerifier{result: registry.BindingVerifyResult(leaked)}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, verifyPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gitlab.example.com:22") || strings.Contains(rec.Body.String(), "ssh:") {
		t.Errorf("response carried the underlying text: %s", rec.Body.String())
	}
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.BindingVerifyNotVerified) {
		t.Errorf("verifyResult = %v, want NOT_VERIFIED for an unregistered verdict", data["verifyResult"])
	}
	if got := reg.clusters["c1"].Git.VerifyResult; got != registry.BindingVerifyNotVerified {
		t.Errorf("stored verifyResult = %q, want NOT_VERIFIED — an unregistered value must not reach the column", got)
	}
}

// 实现返回未登记取值时同样不落库：那次调用没有产出一个可信的结论，
// 而 SetGitVerifyResult 记不了「校验过但结论作废」这件事 —— 它只接受一个
// 具体的时刻。硬写进去，库里与审计里都会多出一次没有发生过的校验。
func TestVerifyGitBindingRecordsNothingForAnUnregisteredVerdict(t *testing.T) {
	var trace []string
	reg := boundRegistry()
	reg.trace = &trace
	stub := &stubGitVerifier{result: registry.BindingVerifyResult("LOOKS_FINE"), trace: &trace}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	if rec := authedPostNoBody(t, h, cookie, verifyPath); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(trace) != 2 || trace[0] != "verify-repo" || trace[1] != "verify" {
		t.Errorf("call sequence = %v, want [verify-repo verify] only — an unusable verdict is not worth recording", trace)
	}
}

// 校验必须发生在落库之前。
//
// 它是一次带秒级超时的出站请求：握着数据库事务等它回来，会把一次网络
// 抖动放大成一次锁竞争故障（spec §4）。「不在事务里」这句话在这一层
// 无法直接断言 —— registry.Store 刻意不暴露事务句柄，httpapi 也就没有
// 事务可以持有 —— 能被断言的是调用顺序：校验在写之前完成。顺序一旦
// 反过来（先写再校验再写第二次），这条测试就红。
func TestVerifyGitBindingRunsBeforeTheStoreWrite(t *testing.T) {
	var trace []string
	reg := boundRegistry()
	reg.trace = &trace
	stub := &stubGitVerifier{result: registry.BindingVerifyOK, trace: &trace}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	if rec := authedPostNoBody(t, h, cookie, verifyPath); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(trace) != 3 || trace[0] != "verify-repo" || trace[1] != "verify" || trace[2] != "set-verdict" {
		t.Errorf("call order = %v, want [verify-repo verify set-verdict]", trace)
	}
}

// 保存路径同样要守这个顺序：出站是在事务之外发生的。
func TestSavingABindingVerifiesBeforeTheStoreWrite(t *testing.T) {
	var trace []string
	reg := unboundRegistry()
	reg.trace = &trace
	stub := &stubGitVerifier{result: registry.BindingVerifyOK, trace: &trace}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(nil))
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	if len(trace) != 3 || trace[0] != "verify-repo" || trace[1] != "verify" || trace[2] != "bind" {
		t.Errorf("call order = %v, want [verify-repo verify bind]", trace)
	}
}

// 库里存着一个今天已经不合法的**仓库**时，重校验说出真实原因，不发出站。
//
// 现实来源：repoUrl 的 SSH 形态校验是后加的，而迁移 6 把绑定里那个
// https:// 地址原样搬进了 git_repo（design doc §6，迁移不做数据修正）。
// 对这种记录做一次出站得到的不是「校验没通过」，而是一句假的结论 ——
// 认证方法与传输对不上，一次拨号都不会发生，失败却会被报成「仓库不可达」
// （spec §2.2）。把这个结论存进库、显示给操作者，比让他直接看到
// 「repoUrl 不是 SSH 形态」要糟得多：后者是他能据以行动的那句话。
//
// 拒绝落在**仓库**上而不是整个集群：集群其余字段不合法不该挡住这条路径
// （见 TestVerifySucceedsWhileTheClusterItselfWouldFailValidation）。
func TestVerifyGitBindingRefusesARepoItCanNoLongerAccept(t *testing.T) {
	reg := boundRegistry()
	repo := boundRepo()
	repo.URL = "https://gitlab.example.com/net/policies.git"
	reg.repos[gitRepoID] = repo
	stub := &stubGitVerifier{result: registry.BindingVerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, verifyPath)
	got := bodyOf(t, rec)
	if got["code"] != float64(20001) {
		t.Fatalf("code = %v, want 20001 (body %s)", got["code"], rec.Body.String())
	}
	if msg, _ := got["msg"].(string); !strings.Contains(msg, "SSH") {
		t.Errorf("msg = %q, want it to name SSH as the real reason", msg)
	}
	if stub.calls != 0 || stub.repoCalls != 0 {
		t.Errorf("verifier calls = %d repo / %d path, want 0 — a verdict that cannot be stored is not worth a handshake",
			stub.repoCalls, stub.calls)
	}
	if stored := reg.clusters["c1"].Git; stored.VerifyResult == registry.BindingVerifyOK {
		t.Error("a rejected re-verify still wrote a verdict")
	}
}

// 绑定指向一个已经不在的仓库时答「不存在」，且不发出站。
//
// 与「集群不存在」同码：从调用方视角都是「要校验的那个东西不在」。
func TestVerifyGitBindingWithAMissingRepoIsBusinessNotFound(t *testing.T) {
	reg := boundRegistry()
	delete(reg.repos, gitRepoID)
	stub := &stubGitVerifier{result: registry.BindingVerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, verifyPath)
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Fatalf("code = %v, want 20002 (body %s)", got, rec.Body.String())
	}
	if stub.repoCalls != 0 || stub.calls != 0 {
		t.Errorf("verifier calls = %d repo / %d path, want 0 — there is no repo to reach", stub.repoCalls, stub.calls)
	}
}

// 仓库级不是 OK 时，路径级结论只能是 NOT_VERIFIED，且不得落库。
//
// 「仓库都没连上却报路径不存在」是一句没有依据的结论，而操作者会照着它
// 去改 policyPath（design doc §3.3）。这一条守的是 handler 这一侧：它必须
// 把**这次**得到的仓库级结论如实传下去，而不是传一个凑手的 OK —— 传 OK
// 的实现在这里会给出 PATH_MISSING，测试就红。
func TestVerifyGitBindingIsNotVerifiedWhenTheRepoLevelIsNotOK(t *testing.T) {
	var trace []string
	reg := boundRegistry()
	reg.trace = &trace
	stub := &stubGitVerifier{
		repoResult: registry.RepoVerifyAuthFailed,
		result:     registry.BindingVerifyPathMissing,
		trace:      &trace,
	}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, verifyPath)
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.BindingVerifyNotVerified) {
		t.Errorf("verifyResult = %v, want NOT_VERIFIED — the repo level did not pass", data["verifyResult"])
	}
	if data["verifiedAt"] != nil {
		t.Errorf("verifiedAt = %v, want absent — this layer never ran", data["verifiedAt"])
	}
	if stub.seenRepoResult != registry.RepoVerifyAuthFailed {
		t.Errorf("path-level got repoResult = %q, want AUTH_FAILED — the verdict this request derived",
			stub.seenRepoResult)
	}
	for _, op := range trace {
		if op == "set-verdict" {
			t.Errorf("call sequence = %v, want no store write — nothing was verified at this layer", trace)
		}
	}
}
