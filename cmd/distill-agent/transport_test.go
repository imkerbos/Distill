package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func opts(url string) options {
	return options{platformURL: url, tokenFile: "/x"}
}

// **明文一律拒绝。** Authorization 头里那把 token 等价于一把能往平台写这个
// 集群全部数据的钥匙 —— 走 http:// 就是让它在集群网络里裸奔。
func TestPlaintextPlatformURLIsRefused(t *testing.T) {
	for _, raw := range []string{
		"http://platform.example",
		"http://10.0.0.5:10100",
		"HTTP://platform.example",
	} {
		if err := opts(raw).validate(); err == nil {
			t.Errorf("%q was accepted; the agent token would cross the network in the clear", raw)
		}
	}
}

// https 照常放行 —— 拒绝明文不等于什么都不接。
func TestHTTPSIsAccepted(t *testing.T) {
	for _, raw := range []string{"https://platform.example", "https://platform.example:8443/base"} {
		if err := opts(raw).validate(); err != nil {
			t.Errorf("%q was refused: %v", raw, err)
		}
	}
}

// 本机开发要有出口，但它必须是**显式**的：一个默认允许明文的实现，
// 生产上没有任何东西会提醒你忘了配 TLS。
func TestPlaintextNeedsAnExplicitOptIn(t *testing.T) {
	o := opts("http://localhost:10100")
	if err := o.validate(); err == nil {
		t.Fatal("plaintext was allowed without opting in")
	}
	o.allowPlaintext = true
	if err := o.validate(); err != nil {
		t.Errorf("plaintext was refused even with the explicit opt-in: %v", err)
	}
}

// 非 http(s) 的 scheme 一律拒。
func TestOnlyHTTPSchemesAreAccepted(t *testing.T) {
	for _, raw := range []string{"ftp://platform.example", "platform.example", "", "://x"} {
		if err := opts(raw).validate(); err == nil {
			t.Errorf("%q was accepted as a platform URL", raw)
		}
	}
}

// **不跟随重定向。**
//
// Go 默认会跟，而 https → http 的**同主机**降级重定向不会剥掉
// Authorization 头 —— 那把 token 于是明文发了出去。摄入端点没有任何理由
// 重定向，因此干脆不跟：一次意外的重定向要以失败的形态出现，而不是以
// 一次成功但泄漏了凭据的推送出现。
func TestTheAgentNeverFollowsARedirect(t *testing.T) {
	var downgraded bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			downgraded = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()

	// 目标写死成 plain 的根路径，不拼请求里的任何东西：拼进来会让 gosec
	// 把这段读成一个开放重定向，而这里要造的只是"平台答了个 307"。
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", plain.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	sink := newHTTPSink(redirector.URL, "dstl_secret")
	err := sink.SaveFlowIngest(t.Context(), flowIngestPayload{RunID: "r-1"})
	if err == nil {
		t.Error("a redirect was followed; an ingest endpoint has no reason to redirect, and a " +
			"same-host downgrade to http keeps the Authorization header")
	}
	if downgraded {
		t.Error("the agent token was sent to the redirect target")
	}
	if err != nil && strings.Contains(err.Error(), plain.URL) {
		t.Error("the error text carries the redirect target; addresses are deployment topology")
	}
}
