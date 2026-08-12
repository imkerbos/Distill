package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// stubGitVerifier 是 httpapi.GitVerifier 的替身。
//
// 它记录调用次数与看到的绑定：一条「结论正确」的断言并不能证明校验器
// 真的被调用过 —— 一个把结论写死的 handler 也能让那条断言通过。两个
// 方向都要有人守。
type stubGitVerifier struct {
	result registry.VerifyResult
	calls  int
	seen   registry.GitBinding
	// trace 与 memRegistry 共用一份调用序列，用于确认校验发生在落库之前。
	trace *[]string
}

func (s *stubGitVerifier) Verify(_ context.Context, b registry.GitBinding) registry.VerifyResult {
	s.calls++
	s.seen = b
	if s.trace != nil {
		*s.trace = append(*s.trace, "verify")
	}
	return s.result
}

// boundCluster 是一个带 Git 绑定的集群，供重校验用例复用。
func boundCluster() registry.Cluster {
	return registry.Cluster{
		ID: "c1", DisplayName: "C1", PodCIDR: "10.4.0.0/14",
		NodeCIDR: "10.128.0.0/20", State: registry.StateReady,
		Git: &registry.GitBinding{
			RepoURL: "https://gitlab.example.com/net/policies.git", Branch: "main",
			PolicyPath: "clusters/c1", CredentialRef: gitBindingRef,
		},
	}
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
	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)

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
	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	stub := &stubGitVerifier{result: registry.VerifyPathMissing}
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
	if stub.seen.RepoURL != boundCluster().Git.RepoURL || stub.seen.CredentialRef != gitBindingRef {
		t.Errorf("verifier saw %+v, want the stored binding", stub.seen)
	}

	data, _ := body["data"].(map[string]any)
	if data["verifyResult"] != string(registry.VerifyPathMissing) {
		t.Errorf("verifyResult = %v, want PATH_MISSING", data["verifyResult"])
	}
	if data["verifiedAt"] == nil {
		t.Error("verifiedAt is absent — a check did happen, and its time is what makes the verdict readable")
	}

	stored := reg.clusters["c1"].Git
	if stored == nil || stored.VerifyResult != registry.VerifyPathMissing {
		t.Fatalf("stored verifyResult = %+v, want PATH_MISSING", stored)
	}
	if stored.VerifiedAt == nil {
		t.Error("stored verifiedAt is nil, want the time of this check")
	}
}

// 未配置校验器时结论是 NOT_VERIFIED，绝不是 OK：没做过的检查不是通过了
// 的检查。这条守的是「缺省即安全」的方向 —— 一个把 nil 当成"没什么可
// 担心的"的实现，会让整个未配置 secrets 的部署显示成全绿。
func TestVerifyGitBindingWithoutAVerifierIsNotVerified(t *testing.T) {
	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPostNoBody(t, h, cookie, verifyPath)
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.VerifyNotVerified) {
		t.Errorf("verifyResult = %v, want NOT_VERIFIED", data["verifyResult"])
	}
	if data["verifiedAt"] != nil {
		t.Errorf("verifiedAt = %v, want absent — no check happened, so there is no such moment", data["verifiedAt"])
	}
	if got := reg.clusters["c1"].Git.VerifyResult; got != registry.VerifyNotVerified {
		t.Errorf("stored verifyResult = %q, want NOT_VERIFIED", got)
	}
}

// 集群不存在与集群没有绑定都是业务失败：没有可校验的对象不是服务出了问题。
func TestVerifyGitBindingWithoutATargetIsBusinessNotFound(t *testing.T) {
	unbound := boundCluster()
	unbound.Git = nil
	reg := newMemRegistry()
	reg.clusters["c1"] = unbound
	stub := &stubGitVerifier{result: registry.VerifyOK}
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
	if stub.calls != 0 {
		t.Errorf("verifier calls = %d, want 0 when there is nothing to verify", stub.calls)
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
			reg := newMemRegistry()
			reg.clusters["c1"] = boundCluster()
			tc.fail(reg)
			stub := &stubGitVerifier{result: registry.VerifyOK}
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

	reg := newMemRegistry()
	reg.clusters["c1"] = boundCluster()
	stub := &stubGitVerifier{result: registry.VerifyResult(leaked)}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPostNoBody(t, h, cookie, verifyPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gitlab.example.com:22") || strings.Contains(rec.Body.String(), "ssh:") {
		t.Errorf("response carried the underlying text: %s", rec.Body.String())
	}
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["verifyResult"] != string(registry.VerifyNotVerified) {
		t.Errorf("verifyResult = %v, want NOT_VERIFIED for an unregistered verdict", data["verifyResult"])
	}
	if got := reg.clusters["c1"].Git.VerifyResult; got != registry.VerifyNotVerified {
		t.Errorf("stored verifyResult = %q, want NOT_VERIFIED — an unregistered value must not reach the column", got)
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
	reg := newMemRegistry()
	reg.trace = &trace
	reg.clusters["c1"] = boundCluster()
	stub := &stubGitVerifier{result: registry.VerifyOK, trace: &trace}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	if rec := authedPostNoBody(t, h, cookie, verifyPath); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(trace) != 2 || trace[0] != "verify" || trace[1] != "update" {
		t.Errorf("call order = %v, want [verify update]", trace)
	}
}

// 保存路径同样要守这个顺序：出站是在事务之外发生的。
func TestSavingABindingVerifiesBeforeTheStoreWrite(t *testing.T) {
	var trace []string
	reg := newMemRegistry()
	reg.trace = &trace
	reg.clusters["c1"] = boundCluster()
	stub := &stubGitVerifier{result: registry.VerifyOK, trace: &trace}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, stub)

	rec := authedPutJSON(t, h, cookie, "/api/v1/clusters/c1", fullClusterBody(nil))
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}
	if len(trace) != 2 || trace[0] != "verify" || trace[1] != "update" {
		t.Errorf("call order = %v, want [verify update]", trace)
	}
}
