package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 401 = 平台明确拒绝这个 agent。必须是**致命**的、可被 errors.Is 认出来的错误 ——
// resolveConfig 据此立刻退出、不重试：token 被吊销了，重试一万次也不会变好，
// 而 CrashLoopBackOff 正是让人去重签 token 的可见信号。
func TestARefusedAgentIsAFatalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":10005}`))
	}))
	defer srv.Close()

	_, err := fetchAgentConfig(t.Context(), srv.Client(), srv.URL, "dstl_revoked")
	if err == nil {
		t.Fatal("a 401 was not reported as an error")
	}
	if !errors.Is(err, errAgentRefused) {
		t.Errorf("a 401 must wrap errAgentRefused so resolveConfig can stop retrying; got %v", err)
	}
}

// 平台暂时不可达（连接错误、5xx）不是致命的 —— resolveConfig 该退避重试，
// 不该让 agent 一崩了之。这里第一次答 503、之后 200，断言最终拿到配置。
func TestResolveConfigRetriesATransientFailure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"clusterId":"c-1","schemaVersion":1}}`))
	}))
	defer srv.Close()

	cfg, err := resolveConfig(t.Context(), srv.Client(), srv.URL, "dstl_x", time.Millisecond, quietLogger(t))
	if err != nil {
		t.Fatalf("a transient 503 was not retried: %v", err)
	}
	if cfg.ClusterID != "c-1" {
		t.Errorf("cfg.ClusterID = %q, want c-1", cfg.ClusterID)
	}
	if hits.Load() < 2 {
		t.Errorf("server hit %d times; the transient failure should have caused a retry", hits.Load())
	}
}

// 401 不重试：一次就退出。重试一把已吊销的 token 只是拿噪音换不来结果。
func TestResolveConfigDoesNotRetryARefusal(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":10005}`))
	}))
	defer srv.Close()

	// 绑一个有界 ctx：正确实现会在 1 次命中后立刻返回，远快于这个预算；
	// 若某次改动把「拒绝」错当暂态去重试，它会打满预算、命中数攀升，这条
	// 用例据此**快速失败**而不是挂死（无界 ctx 会让错误实现无限重试）。
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := resolveConfig(ctx, srv.Client(), srv.URL, "dstl_revoked", time.Millisecond, quietLogger(t))
	if !errors.Is(err, errAgentRefused) {
		t.Fatalf("want errAgentRefused, got %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("server hit %d times; a refusal must not be retried", hits.Load())
	}
}

// 平台一直不可达时，预检预算（ctx）耗尽就放弃并报错 —— 一个持续的故障仍要
// 以退出的形态浮现（CrashLoop），不能让 Pod 显示 Running 却永远什么都不做。
func TestResolveConfigGivesUpWhenTheBudgetRunsOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := resolveConfig(ctx, srv.Client(), srv.URL, "dstl_x", time.Millisecond, quietLogger(t)); err == nil {
		t.Error("a sustained outage must eventually give up and error, not retry forever")
	}
}
