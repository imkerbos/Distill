package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// 漂移检测走到校验器，并把结论原样回传。
//
// 锚点从第四轮起就在写，从来没人读过（design doc 2026-08-18-drift-detection §1）。
// 这条端点是它第一个消费方。
func TestDriftEndpointReturnsTheVerifiersConclusion(t *testing.T) {
	reg := boundRegistry()
	reg.clusters["c1"] = withAnchor(boundCluster())
	gv := &stubGitVerifier{drift: registry.DriftDrifted}
	h, _, cookie := newTestRouterWithGitVerifier(t, fixtureReader(), reg, gv)

	rec := authedGet(t, h, cookie, "/api/v1/clusters/c1/git-binding/drift")
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: %s", got, rec.Body.String())
	}
	if gv.driftCalls != 1 {
		t.Errorf("Drift() called %d times, want 1 — 一个把结论写死的 handler 也能"+
			"让下面那条断言通过", gv.driftCalls)
	}
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["driftResult"] != string(registry.DriftDrifted) {
		t.Errorf("driftResult = %v, want %q", data["driftResult"], registry.DriftDrifted)
	}
	// 校验器必须拿到锚点，否则它只能答 NEVER_WRITTEN。
	if gv.seenAnchor == "" {
		t.Error("the verifier was not given the last written commit")
	}
}

// 没有配置校验器的部署答 UNKNOWN，**不是 IN_SYNC**。
//
// 这是这一轮唯一真正危险的地方：答"一致"会让操作者以为下发的东西还在，
// 而平台根本没有去看过（安全规范 §49）。
func TestDriftWithoutAVerifierIsUnknownNotInSync(t *testing.T) {
	reg := boundRegistry()
	reg.clusters["c1"] = withAnchor(boundCluster())
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg) // 校验器为 nil

	rec := authedGet(t, h, cookie, "/api/v1/clusters/c1/git-binding/drift")
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if data["driftResult"] == string(registry.DriftInSync) {
		t.Fatal("a deployment with no verifier reported IN_SYNC")
	}
	if data["driftResult"] != string(registry.DriftUnknown) {
		t.Errorf("driftResult = %v, want %q", data["driftResult"], registry.DriftUnknown)
	}
}

func TestDriftNeedsABinding(t *testing.T) {
	// 集群不存在与集群没有绑定同码，与 verify 那条一致：从调用方视角
	// 两者都是「要检测的那个东西不在」。
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), fixtureSource())
	rec := authedGet(t, h, cookie, "/api/v1/clusters/c1/git-binding/drift")
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeNotFound) {
		t.Errorf("code = %v, want %d", got, response.CodeNotFound)
	}
}

func TestDriftRequiresAdmin(t *testing.T) {
	// 它会真的发起一次出站克隆，且回传的是策略下发链路的内部状态 ——
	// 与 verify 同一档。
	reg := boundRegistry()
	h, sessions, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	reg.withAccount(t, "alice", registry.RoleViewer, viewerPassword)
	viewer := sessionCookie(t, sessions, reg, "alice", registry.RoleViewer)

	rec := authedGet(t, h, viewer, "/api/v1/clusters/c1/git-binding/drift")
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer status = %d, want 403", rec.Code)
	}
}

// withAnchor 给绑定补一个 last_written_commit，代表"推送过一次"。
func withAnchor(c registry.Cluster) registry.Cluster {
	c.Git.LastWrittenCommit = "0123456789abcdef0123456789abcdef01234567"
	return c
}
