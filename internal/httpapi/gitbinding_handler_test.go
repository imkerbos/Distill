package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// bindingPath 是绑定资源自己的地址：它挂在集群下，但有自己的动词。
const bindingPath = "/api/v1/clusters/c1/git-binding"

// bindingBody 是一个完整的绑定请求体。
//
// extra 里的键会并进对象，用于往请求体里塞那些**不该被采纳**的字段 ——
// 请求体必须能表达它们，测试才证明得了它们没被收下。
func bindingBody(extra map[string]any) map[string]any {
	b := map[string]any{
		"repoUrl": "ssh://git@gitlab.example.com/net/policies.git", "branch": "main",
		"policyPath": "clusters/c1", "credentialRef": gitBindingRef,
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func authedDelete(t *testing.T, h http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// unboundCluster 是一个没有绑定的集群，供绑定用例复用。
func unboundCluster() registry.Cluster {
	c := boundCluster()
	c.Git = nil
	return c
}

// clusterPayload 不再接受 git：不是收下再忽略，而是字段根本不存在。
//
// 一个永远不会被采纳的字段留在请求形状里，只会让下一个调用方以为它有用 ——
// 而在这条路径上，「以为有用」的具体后果是：调用方填了仓库地址、请求返回
// 成功、绑定却从未写下，直到某天有人发现这个集群的策略一直没有下发。
//
// 断言打在替身**拿到的参数**上而不是落库结果上：集群写路径本就会丢弃
// c.Git（绑定不走这条路），只看库里的话，clusterPayload 带不带 git
// 这条测试都是绿的 —— 那正是「不可能失败的测试」的样子。
func TestClusterPayloadNoLongerCarriesTheBinding(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			reg := newMemRegistry()
			reg.clusters["c1"] = unboundCluster()
			stub := &stubGitVerifier{result: registry.VerifyOK}
			h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

			body := fullClusterBody(map[string]any{"git": bindingBody(nil)})
			var rec *httptest.ResponseRecorder
			if method == http.MethodPost {
				body["id"] = "new-5"
				rec = authedPostJSON(t, h, cookie, "/api/v1/clusters", body)
			} else {
				rec = authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", body)
			}
			if got := bodyOf(t, rec)["code"]; got != float64(0) {
				t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
			}

			if reg.lastCluster.Git != nil {
				t.Errorf("the store was handed git = %+v, want nil — the cluster payload has no such field",
					reg.lastCluster.Git)
			}
			if stub.calls != 0 {
				t.Errorf("verifier calls = %d, want 0 — a cluster write has no binding to verify", stub.calls)
			}
			if reg.clusters["c1"].Git != nil {
				t.Errorf("stored git = %+v, want nil", reg.clusters["c1"].Git)
			}
		})
	}
}

func TestBindGitRepoRequiresSession(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = unboundCluster()
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)

	raw, err := json.Marshal(bindingBody(nil))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, bindingPath, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestUnbindGitRepoRequiresSession(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)

	req := httptest.NewRequest(http.MethodDelete, bindingPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if reg.clusters["c1"].Git == nil {
		t.Error("an unauthenticated request removed the binding")
	}
}

// 请求体不是合法 JSON 是协议层问题，不是业务失败 —— 与集群端点一致，
// 必须是真实的 400，否则网关与前端拦截器都要先解析响应体才能分类。
func TestBindGitRepoRejectsMalformedJSON(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = unboundCluster()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	req := httptest.NewRequest(http.MethodPut, bindingPath, bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

// 绑定的四个字段与本次校验的结论都要落库，整体比对而不是挑几个字段断言：
// 新增一个字段却忘记映射时，表现必须是这条测试失败，而不是没有人注意到。
//
// 响应也要带上结论：保存会触发一次校验，而操作者点完保存最想知道的正是
// 那次校验的结果 —— 只落库不回，界面只能再发一次请求才看得到。
func TestBindGitRepoStoresEveryFieldAndTheVerdict(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = unboundCluster()
	stub := &stubGitVerifier{result: registry.VerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", body["code"], rec.Body.String())
	}

	// 校验器确实被调用过，且拿到的是这次请求里的绑定 —— 否则一个把结论
	// 写死的 handler 也能让下面的断言全绿。
	if stub.calls != 1 {
		t.Fatalf("verifier calls = %d, want exactly 1 — a stored verdict nobody derived is a claim, not a fact", stub.calls)
	}
	if stub.seen.PolicyPath != "clusters/c1" || stub.seen.CredentialRef != gitBindingRef {
		t.Errorf("verifier saw %+v, want the binding from the request", stub.seen)
	}

	data, _ := body["data"].(map[string]any)
	if data["verifyResult"] != string(registry.VerifyOK) {
		t.Errorf("verifyResult = %v, want OK", data["verifyResult"])
	}
	if data["verifiedAt"] == nil {
		t.Error("verifiedAt is absent — a check did happen, and its time is what makes the verdict readable")
	}

	stored := reg.clusters["c1"].Git
	if stored == nil {
		t.Fatal("binding was not stored")
	}
	want := registry.GitBinding{
		RepoURL: "ssh://git@gitlab.example.com/net/policies.git", Branch: "main",
		PolicyPath: "clusters/c1", CredentialRef: gitBindingRef,
		VerifyResult: registry.VerifyOK, VerifiedAt: stored.VerifiedAt,
	}
	if *stored != want {
		t.Errorf("stored binding = %+v, want %+v", *stored, want)
	}
	if stored.VerifiedAt == nil {
		t.Error("stored verifiedAt is nil, want the time of this check")
	}
}

// 结论字段不可提交。
//
// verifyResult / verifiedAt 是平台自己校验出来的事实，lastWrittenCommit 是
// 平台对「我最近一次往这个仓库写了什么」的断言。三个都不在请求形状里：
// 一个能被请求体设成 OK 的结论，就是一句无法被证伪的「这个绑定可以下发」；
// 一个能被请求体设定的漂移基准，可以被调成与仓库现状一致，于是平台会带着
// 信心报告「无漂移」，比根本没有基准更糟。
//
// 库里先放一个陈旧的 OK 与一个伪造不来的 SHA，请求体里再塞一个伪造的 OK、
// 一个未来的时间戳和另一个 SHA，而校验器返回 AUTH_FAILED。三个方向一起守：
// 收下请求体里的值会红，沿用库里的旧结论也会红。
func TestBindIgnoresCallerSuppliedVerdictFields(t *testing.T) {
	stale := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	const forgedSHA = "fedcba9876543210fedcba9876543210fedcba98"

	c := boundCluster()
	c.Git.VerifyResult = registry.VerifyOK
	c.Git.VerifiedAt = &stale
	c.Git.LastWrittenCommit = "0123456789abcdef0123456789abcdef01234567"
	reg := newMemRegistry()
	reg.clusters["c1"] = c
	stub := &stubGitVerifier{result: registry.VerifyAuthFailed}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(map[string]any{
		"verifyResult":      string(registry.VerifyOK),
		"verifiedAt":        "2031-01-01T00:00:00Z",
		"lastWrittenCommit": forgedSHA,
	}))
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}

	stored := reg.clusters["c1"].Git
	if stored == nil {
		t.Fatal("binding was not stored")
	}
	if stored.VerifyResult != registry.VerifyAuthFailed {
		t.Errorf("stored verifyResult = %q, want AUTH_FAILED — the verifier's verdict, not the caller's",
			stored.VerifyResult)
	}
	if stored.VerifiedAt == nil || !stored.VerifiedAt.After(stale) {
		t.Errorf("stored verifiedAt = %v, want the time of this check, not the stale one", stored.VerifiedAt)
	}
	if stored.LastWrittenCommit == forgedSHA {
		t.Error("a forged drift baseline was honoured")
	}
	// 响应体同样不得回一个调用方自己填的结论。
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.VerifyAuthFailed) {
		t.Errorf("returned verifyResult = %v, want AUTH_FAILED", data["verifyResult"])
	}
	// 对照组：credentialRef 是操作者填写的 Secret Manager 引用，不是平台
	// 对外部世界的断言，本就该由调用方提供 —— 它必须照常落库，否则这条
	// 防线就从「拦住伪造的断言」变成了「整个请求体都不听调用方的」。
	if stored.CredentialRef != gitBindingRef {
		t.Errorf("credentialRef = %q, want %q — it is caller-supplied by design",
			stored.CredentialRef, gitBindingRef)
	}
}

// 校验失败不阻止保存。
//
// 一次网络抖动不该让操作者无法记录一个正确的绑定：存下来和可信是两件事。
// 断言三样东西同时成立 —— 保存成功、绑定字段完整、结论是那个失败值。
// 少了最后一条，「保存成功」就变成了「未经校验的数据看上去可以下发」。
func TestBindingSucceedsEvenWhenVerificationFails(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = unboundCluster()
	stub := &stubGitVerifier{result: registry.VerifyRepoUnreachable}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 — a failed check must not reject a correct binding", got)
	}
	stored := reg.clusters["c1"].Git
	if stored == nil {
		t.Fatal("binding was not stored")
	}
	if stored.VerifyResult != registry.VerifyRepoUnreachable {
		t.Errorf("stored verifyResult = %q, want REPO_UNREACHABLE", stored.VerifyResult)
	}
	if stored.VerifiedAt == nil {
		t.Error("stored verifiedAt is nil — a check did happen, it just did not pass")
	}
}

// 未配置校验器的部署：保存下来的结论是 NOT_VERIFIED，不是 OK。
//
// 这条与重校验端点那条是同一个方向上的两处调用点 —— 守则正确不等于
// 调用点仍然在调它，两处都要有人守。
func TestBindingWithoutAVerifierRecordsNotVerified(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = unboundCluster()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(nil))
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	stored := reg.clusters["c1"].Git
	if stored == nil || stored.VerifyResult != registry.VerifyNotVerified {
		t.Fatalf("stored verifyResult = %+v, want NOT_VERIFIED — an absent check is not a passed check", stored)
	}
	if stored.VerifiedAt != nil {
		t.Errorf("stored verifiedAt = %v, want nil — no check happened", stored.VerifiedAt)
	}
}

// 非 SSH 的 repoUrl 在保存这一步就被拒绝，且**不发出站**。
//
// 两半都要断言，缺一半都不成立：
//   - 拒绝：一个 https:// 绑定存下来之后没有任何一次校验能通过它，
//     它只会稳定地产出一句「仓库不可达」—— 关于一次从未发生的网络请求
//     的结论（spec §2.2）。
//   - 不发出站：注定被拒的请求体不该先花掉一次带秒级超时的 SSH 握手，
//     否则操作者要等满那个超时，才等到一句和超时无关的报错。
//
// 回传文案必须点名 SSH：只说「地址不合法」会让操作者以为自己打错了字。
func TestBindingANonSSHRepoURLIsRejectedWithoutReachingOut(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = unboundCluster()
	stub := &stubGitVerifier{result: registry.VerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(map[string]any{
		"repoUrl": "https://gitlab.example.com/net/policies.git",
	}))
	got := bodyOf(t, rec)
	if got["code"] != float64(20001) {
		t.Fatalf("code = %v, want 20001 — an https repoUrl is a configuration error, not a verdict", got["code"])
	}
	if msg, _ := got["msg"].(string); !strings.Contains(msg, "SSH") {
		t.Errorf("msg = %q, want it to name SSH as the real reason", msg)
	}
	if stub.calls != 0 {
		t.Errorf("verifier calls = %d, want 0 — a body that cannot be saved must not cost an outbound handshake", stub.calls)
	}
	if reg.clusters["c1"].Git != nil {
		t.Error("a rejected bind still stored a binding")
	}
}

// 请求体本身不合法时同样不发出站 —— 与 repoUrl 无关的字段也算。
//
// 单独一条 https 用例证明不了「先校验后出站」这条时序：它可以被一个
// 「只在 repoUrl 上特判」的实现通过。换一个毫不相干的非法字段再测一次，
// 才能说明拦住出站的是整条绑定校验，不是某一个字段的特例。
func TestBindingAnIncompleteBindingDoesNotCostAnOutboundHandshake(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = unboundCluster()
	stub := &stubGitVerifier{result: registry.VerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(map[string]any{"branch": ""}))
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 for a binding with no branch", got)
	}
	if stub.calls != 0 {
		t.Errorf("verifier calls = %d, want 0 — validation must run before the outbound call", stub.calls)
	}
}

// 集群不存在时两个端点都答「不存在」，且绑定这一侧不发出站。
//
// 一次针对不存在集群的 SSH 握手既救不了这个请求，也会在远端日志里留下
// 一条没人能解释的连接。
func TestGitBindingEndpointsAnswerNotFoundForAnUnknownCluster(t *testing.T) {
	const unknown = "/api/v1/clusters/no-such/git-binding"

	reg := newMemRegistry()
	stub := &stubGitVerifier{result: registry.VerifyOK}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	for _, tc := range []struct {
		name string
		rec  func() *httptest.ResponseRecorder
	}{
		{"bind", func() *httptest.ResponseRecorder {
			return authedPutJSON(t, h, cookie, unknown, bindingBody(nil))
		}},
		{"unbind", func() *httptest.ResponseRecorder { return authedDelete(t, h, cookie, unknown) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.rec()
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 — a missing target is not a server fault", rec.Code)
			}
			if got := bodyOf(t, rec)["code"]; got != float64(20002) {
				t.Errorf("code = %v, want 20002", got)
			}
		})
	}
	if stub.calls != 0 {
		t.Errorf("verifier calls = %d, want 0 — there is nothing to bind to", stub.calls)
	}
}

// 解绑走 DELETE，不再靠「四个字段全空」这种要靠猜的表达。
//
// 未绑定时解绑答「不存在」：从调用方视角，要解绑的那个东西本来就不在 ——
// 与集群不存在同码，界面因此不必区分两种「没有」。
func TestDeleteRemovesTheBindingAndReturnsNotFoundWhenAbsent(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedDelete(t, h, cookie, bindingPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	if reg.clusters["c1"].Git != nil {
		t.Fatalf("git = %+v, want nil after DELETE", reg.clusters["c1"].Git)
	}
	// 集群本身必须原样留下：解绑是解绑，不是下线。
	if reg.clusters["c1"].State != registry.StateReady {
		t.Errorf("state = %q, want READY to survive an unbind", reg.clusters["c1"].State)
	}

	rec = authedDelete(t, h, cookie, bindingPath)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a missing binding is not a server fault", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002 when there is no binding to remove", got)
	}
}

// 拆分买到的东西：集群其余字段不合法，不再妨碍绑定被校验并写下结论。
//
// 现实来源 —— 网段校验是后加的，库里躺着当年存下的、今天已经通不过
// ValidateCluster 的集群行。绑定与集群其余字段的合法性一旦互相牵连，
// 这种集群就再也无法绑定、也无法重新校验：操作者拿到的是一句
// 「podCIDR 不合法」，而他要做的事跟 podCIDR 毫无关系。
//
// 两个调用点都打：绑定保存与手动重校验是两条独立的路径，只测一条的话，
// 另一条上重新出现一道整集群校验也不会有测试变红。
func TestVerifySucceedsWhileTheClusterItselfWouldFailValidation(t *testing.T) {
	broken := boundCluster()
	broken.PodCIDR = "10.4.0/14" // 今天的 ValidateCluster 会拒掉整行
	if err := registry.ValidateCluster(broken); err == nil {
		t.Fatal("the fixture cluster still passes ValidateCluster — this test would prove nothing")
	}

	t.Run("re-verify", func(t *testing.T) {
		reg := newMemRegistry()
		reg.clusters["c1"] = broken
		stub := &stubGitVerifier{result: registry.VerifyOK}
		h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

		rec := authedPostNoBody(t, h, cookie, verifyPath)
		if got := bodyOf(t, rec)["code"]; got != float64(0) {
			t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
		}
		if stub.calls != 1 {
			t.Fatalf("verifier calls = %d, want exactly 1", stub.calls)
		}
		stored := reg.clusters["c1"].Git
		if stored == nil || stored.VerifyResult != registry.VerifyOK {
			t.Fatalf("stored verifyResult = %+v, want OK — a verdict must be recordable on its own", stored)
		}
		if stored.VerifiedAt == nil {
			t.Error("stored verifiedAt is nil, want the time of this check")
		}
		// 结论落库不得把集群其余字段一并带上：那正是嵌在集群写模型里时
		// 付出的代价 —— 一次校验重写了整行，而整行通不过校验。
		if reg.clusters["c1"].PodCIDR != broken.PodCIDR {
			t.Errorf("podCIDR = %q, want it untouched by a verify", reg.clusters["c1"].PodCIDR)
		}
	})

	t.Run("bind", func(t *testing.T) {
		reg := newMemRegistry()
		unbound := broken
		unbound.Git = nil
		reg.clusters["c1"] = unbound
		stub := &stubGitVerifier{result: registry.VerifyOK}
		h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

		rec := authedPutJSON(t, h, cookie, bindingPath, bindingBody(nil))
		if got := bodyOf(t, rec)["code"]; got != float64(0) {
			t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
		}
		stored := reg.clusters["c1"].Git
		if stored == nil || stored.VerifyResult != registry.VerifyOK {
			t.Fatalf("stored binding = %+v, want it saved with an OK verdict", stored)
		}
	})
}
