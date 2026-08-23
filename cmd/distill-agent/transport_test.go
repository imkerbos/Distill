package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// -ca-file 指定的 CA 被真的信任。
//
// 平台证书由内部 CA 签发时，系统根证书里没有它，agent 每一次上报都会以证书
// 校验失败结束 —— 而这个进程在别人的集群里，没有 -ca-file 就没有别的出口。
// 这一条造一个自签的 TLS server，把它的证书写成 PEM 喂给 loadCAPool，断言
// 用得出的 pool 建的 client 能握上手。
func TestACustomCABundleIsTrusted(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	pool, err := loadCAPool(caFile)
	if err != nil || pool == nil {
		t.Fatalf("loadCAPool(%q) = %v, %v; a valid bundle must yield a pool", caFile, pool, err)
	}
	resp, err := agentClient(pool).Get(srv.URL) //nolint:noctx // test
	if err != nil {
		t.Fatalf("the pinned CA was not trusted: %v", err)
	}
	_ = resp.Body.Close()
}

// 没有 -ca-file 时，自签的平台证书被拒 —— 证明那个 pin 真的起作用。
//
// 少了这一条，上一条即便 loadCAPool 什么都没做也可能碰巧绿（比如系统根里
// 恰好信任了它）。这里钉住「不 pin 就连不上」，pin 才是有意义的。
func TestWithoutACABundleTheSelfSignedServerIsRejected(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if resp, err := agentClient(nil).Get(srv.URL); err == nil { //nolint:noctx // test
		_ = resp.Body.Close()
		t.Error("a self-signed platform cert was trusted with only the system roots; " +
			"then -ca-file would be doing nothing")
	}
}

// 一份不是 PEM 的 CA 文件必须被拒，不能静默退回系统根。
//
// 静默退回的后果是：运维以为自己钉了内部 CA，实际走的是系统根 —— 一个
// 内部 CA 签的证书于是被拒，而错误看起来像「网络不通」，查错方向全错。
func TestAGarbageCABundleIsRejected(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCAPool(caFile); err == nil {
		t.Error("a file with no valid PEM certificate was accepted as a CA bundle")
	}
}

// CA 文件读不到时报错，但错误里不带路径 —— 那是部署布局信息（规范 §19、§22）。
func TestAMissingCABundleErrorsWithoutLeakingThePath(t *testing.T) {
	const caPath = "/etc/distill/internal-ca-location.pem"
	_, err := loadCAPool(caPath)
	if err == nil {
		t.Fatal("a missing CA bundle was accepted")
	}
	if strings.Contains(err.Error(), caPath) {
		t.Errorf("the error text carries the CA file path: %q", err.Error())
	}
}

// 空路径 = 不 pin = 用系统根，且不报错：多数部署走公共 CA，-ca-file 是可选的。
func TestAnEmptyCAPathMeansSystemRoots(t *testing.T) {
	pool, err := loadCAPool("")
	if err != nil {
		t.Errorf("an empty -ca-file must not error: %v", err)
	}
	if pool != nil {
		t.Error("an empty -ca-file must yield a nil pool (system roots), not an empty pool")
	}
}

// 共享 client 一律拒重定向 —— 这条纪律现在也盖住了配置预检那条路径，
// 不只是上报。config 请求同样带着 Authorization 头。
func TestTheSharedClientRefusesRedirects(t *testing.T) {
	if agentClient(nil).CheckRedirect == nil {
		t.Fatal("the shared client has no CheckRedirect; a redirect would be followed and the token leaked")
	}
	if err := agentClient(nil).CheckRedirect(nil, nil); err == nil {
		t.Error("the shared client follows redirects")
	}
}
