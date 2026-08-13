package httpapi_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

const (
	reposPath     = "/api/v1/git-repos"
	repoPath      = reposPath + "/" + gitRepoID
	repoVerifyURL = repoPath + "/verify"
)

// repoBody 是一个完整的仓库请求体。
//
// extra 里的键会并进对象，用于往请求体里塞那些**不该被采纳**的字段 ——
// 请求体必须能表达它们，测试才证明得了它们没被收下。
func repoBody(extra map[string]any) map[string]any {
	b := map[string]any{
		"repoId": gitRepoID, "repoUrl": "ssh://git@gitlab.example.com/net/policies.git",
		"branch": "main", "credentialRef": gitBindingRef,
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// 新建仓库要把四个字段整份落库，并**紧接着自动校验一次**（design doc §4）。
//
// 整体比对而不是挑几个字段断言：新增一个字段却忘记映射时，表现必须是这条
// 测试失败，而不是没有人注意到。
//
// 结论也要落库并回传：新建触发了一次校验，而操作者点完保存最想知道的正是
// 那次校验的结果。只落库不回，界面得再发一次请求才看得到。
func TestCreateGitRepoStoresEveryFieldAndVerifiesOnce(t *testing.T) {
	reg := newMemRegistry()
	stub := &stubGitVerifier{repoResult: registry.RepoVerifyAuthFailed}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostJSON(t, h, cookie, reposPath, repoBody(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}

	// 校验器确实被调用过，且拿到的是这次请求里的仓库 —— 否则一个把结论
	// 写死的 handler 也能让下面的断言全绿。
	if stub.repoCalls != 1 {
		t.Fatalf("repo-level calls = %d, want exactly 1 — a stored verdict nobody derived is a claim, not a fact",
			stub.repoCalls)
	}
	// 新建不做路径级校验：仓库还没有被任何集群绑定，也就没有 policyPath
	// 可谈 —— 一次凭空的路径查找回答的是一个没人问过的问题。
	if stub.calls != 0 {
		t.Errorf("path-level calls = %d, want 0 — a repo has no policy path of its own", stub.calls)
	}

	stored, ok := reg.repos[gitRepoID]
	if !ok {
		t.Fatal("repo was not stored")
	}
	want := registry.GitRepo{
		ID: gitRepoID, URL: "ssh://git@gitlab.example.com/net/policies.git",
		Branch: "main", CredentialRef: gitBindingRef,
		VerifyResult: registry.RepoVerifyAuthFailed, VerifiedAt: stored.VerifiedAt,
	}
	if stored != want {
		t.Errorf("stored repo = %+v, want %+v", stored, want)
	}
	if stored.VerifiedAt == nil {
		t.Error("stored verifiedAt is nil, want the time of this check")
	}

	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.RepoVerifyAuthFailed) {
		t.Errorf("verifyResult = %v, want AUTH_FAILED", data["verifyResult"])
	}
	if data["verifiedAt"] == nil {
		t.Error("verifiedAt is absent — a check did happen, and its time is what makes the verdict readable")
	}
}

// 结论字段在两层的请求形状里都不存在。
//
// verifyResult / verifiedAt 是平台自己校验出来的事实，lastWrittenCommit 是
// 平台对「我最近一次往这个仓库写了什么」的断言。一个能被请求体设成 OK 的
// 结论，就是一句无法被证伪的「这里可以下发」。
//
// 两层各打一遍，因为它们是两个独立的请求形状：只测其中一层的话，另一层
// 重新长出一个 verifyResult 字段也不会有测试变红。三个方向一起守 ——
// 收下请求体里的值会红，沿用库里的旧结论也会红，回传给调用方也会红。
func TestVerdictFieldsAreAbsentFromEveryRequestShape(t *testing.T) {
	const forgedSHA = "fedcba9876543210fedcba9876543210fedcba98"
	forged := map[string]any{
		"verifyResult":      "OK",
		"verifiedAt":        "2031-01-01T00:00:00Z",
		"lastWrittenCommit": forgedSHA,
	}

	t.Run("repo create", func(t *testing.T) {
		reg := newMemRegistry()
		stub := &stubGitVerifier{repoResult: registry.RepoVerifyBranchMissing}
		h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

		rec := authedPostJSON(t, h, cookie, reposPath, repoBody(forged))
		if got := bodyOf(t, rec)["code"]; got != float64(0) {
			t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
		}
		// 交给 store 的那份里就不该有结论：只看落库结果的话，一个真实实现
		// 会把空结论落成 NOT_VERIFIED，于是伪造的 OK 也看不出来。
		if reg.lastRepo.VerifyResult != "" || reg.lastRepo.VerifiedAt != nil {
			t.Errorf("the store was handed verifyResult=%q verifiedAt=%v, want neither — the payload has no such fields",
				reg.lastRepo.VerifyResult, reg.lastRepo.VerifiedAt)
		}
		if got := reg.repos[gitRepoID].VerifyResult; got != registry.RepoVerifyBranchMissing {
			t.Errorf("stored verifyResult = %q, want BRANCH_MISSING — the verifier's verdict, not the caller's", got)
		}
	})

	t.Run("repo update", func(t *testing.T) {
		reg := boundRegistry()
		reg.repos[gitRepoID] = registry.GitRepo{
			ID: gitRepoID, URL: "ssh://git@gitlab.example.com/net/policies.git",
			Branch: "main", CredentialRef: gitBindingRef,
			VerifyResult: registry.RepoVerifyOK,
		}
		stub := &stubGitVerifier{}
		h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

		rec := authedPutJSON(t, h, cookie, repoPath, repoBody(forged))
		if got := bodyOf(t, rec)["code"]; got != float64(0) {
			t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
		}
		if reg.lastRepo.VerifyResult != "" || reg.lastRepo.VerifiedAt != nil {
			t.Errorf("the store was handed verifyResult=%q verifiedAt=%v, want neither",
				reg.lastRepo.VerifyResult, reg.lastRepo.VerifiedAt)
		}
		// 改配置之后旧结论必须作废：换了地址之后，旧的 OK 描述的是另一个
		// 仓库，留着它就是拿一句关于别处的判断给新地址背书。
		if got := reg.repos[gitRepoID].VerifyResult; got != registry.RepoVerifyNotVerified {
			t.Errorf("stored verifyResult = %q, want NOT_VERIFIED after an edit", got)
		}
	})

	t.Run("binding", func(t *testing.T) {
		reg := unboundRegistry()
		stub := &stubGitVerifier{result: registry.BindingVerifyPathMissing}
		h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

		rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(forged))
		if got := bodyOf(t, rec)["code"]; got != float64(0) {
			t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
		}
		stored := reg.clusters["c1"].Git
		if stored == nil {
			t.Fatal("binding was not stored")
		}
		if stored.VerifyResult != registry.BindingVerifyPathMissing {
			t.Errorf("stored verifyResult = %q, want PATH_MISSING — the verifier's verdict, not the caller's",
				stored.VerifyResult)
		}
		if stored.LastWrittenCommit == forgedSHA {
			t.Error("a forged drift baseline was honoured")
		}
		data, _ := bodyOf(t, rec)["data"].(map[string]any)
		if data["verifyResult"] != string(registry.BindingVerifyPathMissing) {
			t.Errorf("returned verifyResult = %v, want PATH_MISSING", data["verifyResult"])
		}
	})
}

// 非 SSH 的 repoUrl 在保存这一步就被拒绝，且**不发出站**。
//
// 两半都要断言，缺一半都不成立：
//   - 拒绝：一个 https:// 仓库存下来之后没有任何一次校验能通过它，它只会
//     稳定地产出一句「仓库不可达」—— 关于一次从未发生的网络请求的结论
//     （spec §2.2）。
//   - 不发出站：注定被拒的请求体不该先花掉一次带秒级超时的 SSH 握手。
//
// 回传文案必须点名 SSH：只说「地址不合法」会让操作者以为自己打错了字。
func TestCreateGitRepoRejectsANonSSHURLWithoutReachingOut(t *testing.T) {
	reg := newMemRegistry()
	stub := &stubGitVerifier{}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostJSON(t, h, cookie, reposPath, repoBody(map[string]any{
		"repoUrl": "https://gitlab.example.com/net/policies.git",
	}))
	got := bodyOf(t, rec)
	if got["code"] != float64(20001) {
		t.Fatalf("code = %v, want 20001 — an https repoUrl is a configuration error, not a verdict", got["code"])
	}
	if msg, _ := got["msg"].(string); !strings.Contains(msg, "SSH") {
		t.Errorf("msg = %q, want it to name SSH as the real reason", msg)
	}
	if stub.repoCalls != 0 {
		t.Errorf("verifier calls = %d, want 0 — a body that cannot be saved must not cost an outbound handshake",
			stub.repoCalls)
	}
	if _, ok := reg.repos[gitRepoID]; ok {
		t.Error("a rejected create still stored a repo")
	}
}

// 仓库 ID 是不可改键：PUT 改的是字段，永远不是 id（controller 裁定）。
//
// 允许改它，等于让一次「改地址」顺手把审计里 git-repo/<旧 ID> 那一串行
// 变成无主记录 —— 而审计行正是事后唯一能回答「这个仓库的凭据是谁换的」
// 的东西。
//
// 断言打在替身**拿到的参数**上：只看库里的话，一个把请求体 repoId 传下去
// 的实现会因为「那个 ID 不存在」而返回 20002，测试照样红，但红的理由是
// 另一件事；打在参数上，红的理由才是「它试图改键」。
func TestUpdateGitRepoNeverRekeysTheRepository(t *testing.T) {
	reg := boundRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, repoPath, repoBody(map[string]any{
		"repoId": "some-other-id",
		"branch": "release",
	}))
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	if reg.lastRepo.ID != gitRepoID {
		t.Errorf("the store was handed repoId = %q, want %q — the id comes from the path, never the body",
			reg.lastRepo.ID, gitRepoID)
	}
	if _, ok := reg.repos["some-other-id"]; ok {
		t.Error("a second repo appeared under the id from the request body")
	}
	if got := reg.repos[gitRepoID].Branch; got != "release" {
		t.Errorf("branch = %q, want the edit to have landed on the repo named by the path", got)
	}
}

// 删除一个仍被绑定的仓库必须被拒绝，且是一次**业务失败**，不是 500。
//
// 不做级联（design doc §4）：级联会让一次仓库清理静默解除某个集群的策略
// 下发路径 —— 没有报错、没有人做过「这个集群不再下发策略」的决定，下一次
// 推荐照常产出，只是再也没有地方接收它。
//
// 走 500 的话，界面上只剩一句「服务内部错误」，操作者不会知道该先去解除
// 那个绑定；而这次失败也会计入服务错误率，是一条假的故障信号。
//
// 底层错误文本一个字都不能进响应体：它由存储层拼出，里面带着仓库 ID 与
// 集群 ID（规范 §19、§22）。
func TestDeleteGitRepoRefusesARepoThatIsStillBound(t *testing.T) {
	reg := boundRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedDelete(t, h, cookie, repoPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a refused delete is a business failure, not a server fault", rec.Code)
	}
	got := bodyOf(t, rec)
	if got["code"] != float64(response.CodeInvalidParam) {
		t.Fatalf("code = %v, want %d (body %s)", got["code"], response.CodeInvalidParam, rec.Body.String())
	}
	if msg, _ := got["msg"].(string); !strings.Contains(msg, "绑定") {
		t.Errorf("msg = %q, want it to say the repo is still bound — that is the action the operator must take", msg)
	}
	// 存储层拼的那句话带着仓库 ID 与集群 ID，一个字都不该出现在响应里。
	for _, leaked := range []string{"still bound", "cluster c1"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Errorf("response carried the underlying error text %q: %s", leaked, rec.Body.String())
		}
	}
	if _, ok := reg.repos[gitRepoID]; !ok {
		t.Error("the repo was deleted anyway")
	}
}

// 没有绑定时删除照常成功 —— 上一条的对照组。
//
// 少了它，一个「无论如何都拒绝删除」的实现在上一条下也是绿的。
func TestDeleteGitRepoRemovesAnUnboundRepo(t *testing.T) {
	reg := boundRegistry()
	unbound := boundCluster()
	unbound.Git = nil
	reg.clusters["c1"] = unbound
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedDelete(t, h, cookie, repoPath)
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	if _, ok := reg.repos[gitRepoID]; ok {
		t.Error("the repo is still there after a successful delete")
	}

	// 再删一次是「不存在」，不是服务故障。
	rec = authedDelete(t, h, cookie, repoPath)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNotFound) {
		t.Errorf("code = %v, want %d when there is no repo to remove", got, response.CodeNotFound)
	}
}

// 重新校验的结论必须同时进响应与库：只回不落库，界面刷新一次就退回旧结论；
// 只落库不回，操作者点完看不到任何变化。
func TestVerifyGitRepoStoresAndReturnsTheVerdict(t *testing.T) {
	reg := boundRegistry()
	stub := &stubGitVerifier{repoResult: registry.RepoVerifyCredentialUnresolved}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, repoVerifyURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if stub.repoCalls != 1 {
		t.Fatalf("verifier calls = %d, want exactly 1", stub.repoCalls)
	}
	if stub.seenRepo != boundRepo() {
		t.Errorf("verifier saw %+v, want the stored repo", stub.seenRepo)
	}

	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.RepoVerifyCredentialUnresolved) {
		t.Errorf("verifyResult = %v, want CREDENTIAL_UNRESOLVED", data["verifyResult"])
	}
	if data["verifiedAt"] == nil {
		t.Error("verifiedAt is absent — a check did happen, and its time is what makes the verdict readable")
	}

	stored := reg.repos[gitRepoID]
	if stored.VerifyResult != registry.RepoVerifyCredentialUnresolved {
		t.Errorf("stored verifyResult = %q, want CREDENTIAL_UNRESOLVED", stored.VerifyResult)
	}
	// 跑一次校验不是一次配置变更：地址、分支与凭据引用一个都不许动。
	if stored.URL != boundRepo().URL || stored.Branch != boundRepo().Branch ||
		stored.CredentialRef != boundRepo().CredentialRef {
		t.Errorf("stored repo = %+v, want a verify to touch only the verdict and its time", stored)
	}
}

// 未配置校验器时结论是 NOT_VERIFIED，绝不是 OK：没做过的检查不是通过了
// 的检查。一次没有发生的校验也不该留下任何写入 —— 落库就等于宣称某时某刻
// 校验过一次，并留下一条描述空白事件的 VERIFY_GIT_REPO 审计行。
func TestVerifyGitRepoWithoutAVerifierIsNotVerified(t *testing.T) {
	var trace []string
	reg := boundRegistry()
	reg.trace = &trace
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostNoBody(t, h, cookie, repoVerifyURL)
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.RepoVerifyNotVerified) {
		t.Errorf("verifyResult = %v, want NOT_VERIFIED", data["verifyResult"])
	}
	if data["verifiedAt"] != nil {
		t.Errorf("verifiedAt = %v, want absent — no check happened, so there is no such moment", data["verifiedAt"])
	}
	if len(trace) != 0 {
		t.Errorf("store writes = %v, want none — nothing was verified, so there is nothing to record", trace)
	}
}

// 结论字段不得成为自由文本的载体。
//
// GitVerifier 是一个接口，任何实现都可能返回一个未登记的取值 —— 而那个值
// 在这条链路上最可能的来源，正是某个底层报错的文本（go-git、SSH、凭据
// 解析）。它会同时写进 verify_result 列与响应体，于是「边界不透传底层
// 错误」这句话就只剩下 gitverify 一个包在兜。边界必须自己是结构化的。
func TestVerifyGitRepoNeverPutsFreeTextOnTheWire(t *testing.T) {
	const leaked = "ssh: handshake failed for git@gitlab.example.com:22"

	var trace []string
	reg := boundRegistry()
	reg.trace = &trace
	stub := &stubGitVerifier{repoResult: registry.RepoVerifyResult(leaked), trace: &trace}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, repoVerifyURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gitlab.example.com:22") || strings.Contains(rec.Body.String(), "ssh:") {
		t.Errorf("response carried the underlying text: %s", rec.Body.String())
	}
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.RepoVerifyNotVerified) {
		t.Errorf("verifyResult = %v, want NOT_VERIFIED for an unregistered verdict", data["verifyResult"])
	}
	if got := reg.repos[gitRepoID].VerifyResult; got != registry.RepoVerifyNotVerified {
		t.Errorf("stored verifyResult = %q, want the column untouched by an unregistered value", got)
	}
	// 未登记的结论同样不落库：那次调用没有产出一个可信的结论，而
	// SetGitRepoVerifyResult 记不了「校验过但结论作废」这件事。
	if len(trace) != 1 || trace[0] != "verify-repo" {
		t.Errorf("call sequence = %v, want [verify-repo] only — an unusable verdict is not worth recording", trace)
	}
}

// 校验必须发生在落库之前：它是一次带秒级超时的出站请求，握着数据库事务
// 等它回来，会把一次网络抖动放大成一次锁竞争故障。
func TestVerifyGitRepoRunsBeforeTheStoreWrite(t *testing.T) {
	var trace []string
	reg := boundRegistry()
	reg.trace = &trace
	stub := &stubGitVerifier{repoResult: registry.RepoVerifyOK, trace: &trace}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	if rec := authedPostNoBody(t, h, cookie, repoVerifyURL); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(trace) != 2 || trace[0] != "verify-repo" || trace[1] != "set-repo-verdict" {
		t.Errorf("call order = %v, want [verify-repo set-repo-verdict]", trace)
	}
}

// 库里存着一个今天已经不合法的仓库时，重校验说出真实原因，不发出站。
//
// 现实来源：迁移 6 把绑定里那个 https:// 地址原样搬进了 git_repo
// （design doc §6，迁移不做数据修正）。
func TestVerifyGitRepoRefusesARecordItCanNoLongerAccept(t *testing.T) {
	reg := boundRegistry()
	repo := boundRepo()
	repo.URL = "https://gitlab.example.com/net/policies.git"
	reg.repos[gitRepoID] = repo
	stub := &stubGitVerifier{repoResult: registry.RepoVerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, repoVerifyURL)
	got := bodyOf(t, rec)
	if got["code"] != float64(20001) {
		t.Fatalf("code = %v, want 20001 (body %s)", got["code"], rec.Body.String())
	}
	if msg, _ := got["msg"].(string); !strings.Contains(msg, "SSH") {
		t.Errorf("msg = %q, want it to name SSH as the real reason", msg)
	}
	if stub.repoCalls != 0 {
		t.Errorf("verifier calls = %d, want 0 — a verdict that cannot be trusted is not worth a handshake", stub.repoCalls)
	}
	if reg.repos[gitRepoID].VerifyResult == registry.RepoVerifyOK {
		t.Error("a rejected re-verify still wrote a verdict")
	}
}

// 目标不存在是业务失败：没有可操作的对象不是服务出了问题。
func TestGitRepoEndpointsAnswerNotFoundForAnUnknownRepo(t *testing.T) {
	const unknown = reposPath + "/no-such"

	reg := newMemRegistry()
	stub := &stubGitVerifier{}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	for _, tc := range []struct {
		name string
		rec  func() *httptest.ResponseRecorder
	}{
		{"update", func() *httptest.ResponseRecorder {
			return authedPutJSON(t, h, cookie, unknown, repoBody(nil))
		}},
		{"delete", func() *httptest.ResponseRecorder { return authedDelete(t, h, cookie, unknown) }},
		{"verify", func() *httptest.ResponseRecorder { return authedPostNoBody(t, h, cookie, unknown+"/verify") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.rec()
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 — a missing target is not a server fault", rec.Code)
			}
			if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNotFound) {
				t.Errorf("code = %v, want %d", got, response.CodeNotFound)
			}
		})
	}
	if stub.repoCalls != 0 {
		t.Errorf("verifier calls = %d, want 0 — there is no repo to reach", stub.repoCalls)
	}
}

func TestListGitReposReadsRegistry(t *testing.T) {
	reg := boundRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedGet(t, h, cookie, reposPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	data, ok := bodyOf(t, rec)["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v, want one repo from the registry", bodyOf(t, rec)["data"])
	}
	// 空列表回 []，不是 null：前端不必为两种「没有」各写一段。
	h2, _, cookie2 := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())
	emptyBody := bodyOf(t, authedGet(t, h2, cookie2, reposPath))
	if got, ok := emptyBody["data"].([]any); !ok || len(got) != 0 {
		t.Errorf("data = %v for an empty registry, want []", emptyBody["data"])
	}
}

func TestGitRepoEndpointsRequireSession(t *testing.T) {
	reg := boundRegistry()
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, reposPath},
		{http.MethodPost, reposPath},
		{http.MethodPut, repoPath},
		{http.MethodDelete, repoPath},
		{http.MethodPost, repoVerifyURL},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	if _, ok := reg.repos[gitRepoID]; !ok {
		t.Error("an unauthenticated request removed the repo")
	}
}

func TestCreateGitRepoRejectsMalformedJSON(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), newMemRegistry())

	req := httptest.NewRequest(http.MethodPost, reposPath, bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

// registry 内部故障走真实的 500，错误细节一个字都不能进响应体。
//
// 读与写两条分支都要打：重校验先读后写，只让整个替身失败的话，请求会停在
// 读那一步，写路径上的错误处理根本没被执行。
func TestGitRepoEndpointsDoNotLeakRegistryErrorText(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*memRegistry)
	}{
		{"read", func(m *memRegistry) { m.failWith = errTestRegistry }},
		{"write", func(m *memRegistry) { m.failWritesWith = errTestRegistry }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := boundRegistry()
			tc.fail(reg)
			stub := &stubGitVerifier{repoResult: registry.RepoVerifyOK}
			h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

			rec := authedPostNoBody(t, h, cookie, repoVerifyURL)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
			}
			if got := bodyOf(t, rec)["msg"]; got != response.CodeInternal.Message() {
				t.Errorf("msg = %q, want the fixed internal-error message", got)
			}
			for _, secret := range []string{"mysql", "10.0.0.5", "3306"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("response leaked %q: %s", secret, rec.Body.String())
				}
			}
		})
	}
}

// 结论落库时带的时刻必须是这次校验发生的那个，不是一个零值。
//
// 零值 time.Time 是 1970 年，任何新鲜度检查都会把它当成「校验过」放行。
func TestVerifyGitRepoRecordsTheMomentOfTheCheck(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	reg := boundRegistry()
	stub := &stubGitVerifier{repoResult: registry.RepoVerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	if rec := authedPostNoBody(t, h, cookie, repoVerifyURL); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	at := reg.repos[gitRepoID].VerifiedAt
	if at == nil || !at.After(before) {
		t.Errorf("stored verifiedAt = %v, want the moment of this check (after %v)", at, before)
	}
}
