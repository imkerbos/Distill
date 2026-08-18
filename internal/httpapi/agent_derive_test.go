package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// recordingDeriver 记下摄入之后发生的推导。
type recordingDeriver struct {
	locked  []string
	derived []derivedRun
	saved   []snapshotstore.DeriveRun
	// lockErr / deriveErr 让两段分别失败：它们的处置完全不同 ——
	// 拿不到锁的处置是重试，推导本身失败的处置是去查库。
	lockErr   error
	deriveErr error
	released  int
}

type derivedRun struct{ clusterID, runID string }

func (d *recordingDeriver) LockCluster(_ context.Context, clusterID string) (func(context.Context) error, error) {
	if d.lockErr != nil {
		return nil, d.lockErr
	}
	d.locked = append(d.locked, clusterID)
	return func(context.Context) error { d.released++; return nil }, nil
}

func (d *recordingDeriver) DeriveIdentityIntervals(_ context.Context, clusterID, runID string) error {
	d.derived = append(d.derived, derivedRun{clusterID, runID})
	return d.deriveErr
}

func (d *recordingDeriver) SaveDeriveRun(_ context.Context, run snapshotstore.DeriveRun) error {
	d.saved = append(d.saved, run)
	return nil
}

func TestIngestDerivesIdentityForTheRunItJustStored(t *testing.T) {
	// **没有这一步，推送式接入的集群是不可用的**：pod_identity_interval 会是
	// 空的，而六个读方法每一个都从那张表出发 —— 页面上是「这个集群还没有
	// 可用的采集数据」，而资产其实已经在库里了。
	sink := &recordingSink{}
	deriver := &recordingDeriver{}
	h, _, cookie := newTestRouterWithAgentPipeline(t, sink, deriver)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	if got := bodyOf(t, postRun(t, h, token, runBody(``)))["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0", got)
	}
	if len(deriver.derived) != 1 {
		t.Fatalf("derivations = %d, want 1", len(deriver.derived))
	}
	got := deriver.derived[0]
	if got.clusterID != "prod-asia-1" || got.runID != "r-1" {
		t.Errorf("derived (%q, %q), want (prod-asia-1, r-1)", got.clusterID, got.runID)
	}
}

func TestIngestTakesTheClusterLockBeforeDeriving(t *testing.T) {
	// 互斥不是可选的：同一集群的两次**不同**运行并发推导时走 UPDATE 路径，
	// 只在行锁上排队，后写的赢且不报任何错 —— 一个仍在运行的 Pod 的区间
	// 被关在错的时刻，之后它的地址归属到另一个工作负载，且不报错。
	sink := &recordingSink{}
	deriver := &recordingDeriver{}
	h, _, cookie := newTestRouterWithAgentPipeline(t, sink, deriver)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	postRun(t, h, token, runBody(``))

	if len(deriver.locked) != 1 || deriver.locked[0] != "prod-asia-1" {
		t.Errorf("locked = %v, want exactly [prod-asia-1]", deriver.locked)
	}
	if deriver.released != 1 {
		t.Errorf("released = %d, want 1 — 锁没放掉，这个集群之后再也推导不了",
			deriver.released)
	}
}

func TestIngestFailsWhenDerivationFailsSoTheRetryHeals(t *testing.T) {
	// 答成功而身份没推出来，界面上是一次完全正常的采集，而这个集群的每一条
	// 连接都归属不了。答失败让 agent 重推，而重推会再推导一次 —— 顺序重推
	// 是幂等的（snapshotstore 那条集成用例钉着这一点），于是这条路径自愈。
	sink := &recordingSink{}
	deriver := &recordingDeriver{deriveErr: errors.New("derive blew up")}
	h, _, cookie := newTestRouterWithAgentPipeline(t, sink, deriver)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postRun(t, h, token, runBody(``))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	// 失败照样记账：一次「资产有了、身份没有」的运行必须在库里留下痕迹，
	// 否则界面上它与一次完整的采集分不开。
	if len(deriver.saved) != 1 || deriver.saved[0].Status != snapshotstore.DeriveFailed {
		t.Errorf("saved = %+v, want one FAILED derive run", deriver.saved)
	}
}

func TestIngestDerivesAgainWhenTheRunWasAlreadyStored(t *testing.T) {
	// 上一次推导失败 → agent 重推 → Save 答「已存过」。**这时必须再推一次**：
	// 直接答成功会让那次失败的推导永远补不上，而那个集群会一直停在
	// 「资产有了、身份没有」的状态。
	sink := &recordingSink{err: snapshotstore.ErrRunExists}
	deriver := &recordingDeriver{}
	h, _, cookie := newTestRouterWithAgentPipeline(t, sink, deriver)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	if got := bodyOf(t, postRun(t, h, token, runBody(``)))["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0", got)
	}
	if len(deriver.derived) != 1 {
		t.Errorf("derivations = %d, want 1 — 重推没有补上那次失败的推导",
			len(deriver.derived))
	}
}

func TestIngestReportsAContendedDerivationInsteadOfSkippingIt(t *testing.T) {
	// 拿不到锁不是「跳过」：不留下这一行，一次「资产有了、身份没有」的运行
	// 在界面上完全正常。处置也不同 —— LOCK_UNAVAILABLE 的处置是重跑。
	sink := &recordingSink{}
	deriver := &recordingDeriver{lockErr: snapshotstore.ErrDeriveInProgress}
	h, _, cookie := newTestRouterWithAgentPipeline(t, sink, deriver)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postRun(t, h, token, runBody(``))
	// 争用不是服务故障，答业务码（见 TestIngestNamesAContendedDerivationTheSameWay）。
	// 这条用例盯的是另一半：**它必须被记下来**，不是被跳过。
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeConcurrentCollection) {
		t.Errorf("code = %v, want %d", got, response.CodeConcurrentCollection)
	}
	if len(deriver.saved) != 1 {
		t.Fatalf("saved = %+v, want one recorded derive run", deriver.saved)
	}
	if got := deriver.saved[0].ErrorReason; got != snapshotstore.DeriveErrorLockUnavailable {
		t.Errorf("reason = %q, want %q", got, snapshotstore.DeriveErrorLockUnavailable)
	}
}

func TestAbortedRunsAreNotDerived(t *testing.T) {
	// 一轮没能开始的运行没有任何观测，推导它只会拿一份空事实去关区间 ——
	// 而「不完整的采集不得关闭区间」是 C 轮的核心约束。
	sink := &recordingSink{}
	deriver := &recordingDeriver{}
	h, _, cookie := newTestRouterWithAgentPipeline(t, sink, deriver)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	body := `{"schemaVersion":1,"runId":"r-aborted","status":"FAILED",
	          "errorReason":"CREDENTIAL_UNAVAILABLE",
	          "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:01Z",
	          "observedAt":"2026-08-18T00:00:01Z","observation":{"pods":[]}}`
	if got := bodyOf(t, postRun(t, h, token, body))["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0", got)
	}
	if len(deriver.derived) != 0 {
		t.Errorf("derivations = %d, want none", len(deriver.derived))
	}
}

func TestIngestNamesAConcurrentCollectionInsteadOfBlamingItself(t *testing.T) {
	// **这条用例来自一次真实的并发演练**：两个 agent 同时打同一个集群，
	// 其中一个整份被拒，收到的是裸的「服务内部错误」——操作者会去查平台，
	// 而平台什么问题都没有：成因是这个集群同时跑着两个采集器。
	sink := &recordingSink{err: snapshotstore.ErrObservationExists}
	deriver := &recordingDeriver{}
	h, _, cookie := newTestRouterWithAgentPipeline(t, sink, deriver)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postRun(t, h, token, runBody(``))
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeConcurrentCollection) {
		t.Errorf("code = %v, want %d: %s", got, response.CodeConcurrentCollection, rec.Body.String())
	}
	// 落不进去就不该推导：那一刻的观测属于另一次运行。
	if len(deriver.derived) != 0 {
		t.Errorf("derivations = %d, want none — 推导了一份没有落库的观测",
			len(deriver.derived))
	}
}

func TestIngestNamesAContendedDerivationTheSameWay(t *testing.T) {
	// 推导拿不到互斥，与观测撞车是同一件事的两个阶段：这个集群同时跑着
	// 两个采集器。对操作者是同一句话、同一个处置，因此同一个码。
	sink := &recordingSink{}
	deriver := &recordingDeriver{lockErr: snapshotstore.ErrDeriveInProgress}
	h, _, cookie := newTestRouterWithAgentPipeline(t, sink, deriver)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postRun(t, h, token, runBody(``))
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeConcurrentCollection) {
		t.Errorf("code = %v, want %d: %s", got, response.CodeConcurrentCollection, rec.Body.String())
	}
}
