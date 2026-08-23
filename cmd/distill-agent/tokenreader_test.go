package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileTokenReader 每次都从文件现读 —— token 轮换后不必重启就能被拾起。
func TestFileTokenReaderPicksUpARotatedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("dstl_first"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := fileTokenReader(path)

	got, err := read()
	if err != nil || got != "dstl_first" {
		t.Fatalf("read() = %q, %v; want dstl_first", got, err)
	}

	// kubelet 更新挂载的 Secret 后，文件内容变了；下一次读必须拿到新的。
	if err := os.WriteFile(path, []byte("dstl_second"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = read()
	if err != nil || got != "dstl_second" {
		t.Errorf("read() after rotation = %q, %v; want dstl_second — a rotated token was not picked up", got, err)
	}
}

// 挂载会 trim 掉 Secret 结尾常见的换行；空文件必须报错，不能当成一把空 token 发出去。
func TestFileTokenReaderTrimsAndRejectsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  dstl_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := fileTokenReader(path)(); got != "dstl_x" {
		t.Errorf("read() = %q; want trimmed dstl_x", got)
	}
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fileTokenReader(path)(); err == nil {
		t.Error("an empty token file was accepted; an empty Authorization would be sent")
	}
}

// 读不到时报错，且错误里不带路径（规范 §19、§22）。
func TestFileTokenReaderDoesNotLeakThePath(t *testing.T) {
	const p = "/var/run/secrets/distill-agent-token-secret-location"
	_, err := fileTokenReader(p)()
	if err == nil {
		t.Fatal("a missing token file was accepted")
	}
	if strings.Contains(err.Error(), p) {
		t.Errorf("the error text carries the token path: %q", err.Error())
	}
}

// **热轮换的行为验证**：sink 用 fileTokenReader，轮换文件之后，下一次上报
// 带的是新 token —— 全程没有重启。
func TestTheSinkSendsWhicheverTokenIsCurrent(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("dstl_old"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := newHTTPSinkReading(srv.URL, fileTokenReader(path), srv.Client())

	if err := sink.SaveFlowIngest(t.Context(), flowIngestPayload{RunID: "r-1"}); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// 轮换。不重启，不重建 sink。
	if err := os.WriteFile(path, []byte("dstl_new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sink.SaveFlowIngest(t.Context(), flowIngestPayload{RunID: "r-2"}); err != nil {
		t.Fatalf("second push: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("saw %d requests, want 2", len(seen))
	}
	if seen[0] != "Bearer dstl_old" {
		t.Errorf("first push carried %q, want Bearer dstl_old", seen[0])
	}
	if seen[1] != "Bearer dstl_new" {
		t.Errorf("second push carried %q, want Bearer dstl_new — the rotated token was not picked up "+
			"without a restart", seen[1])
	}
}

// token 读不到时，那一次上报必须失败，不能带一个空 Authorization 发出去。
func TestASendWithAnUnreadableTokenFails(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	sink := newHTTPSinkReading(srv.URL, fileTokenReader("/no/such/token"), srv.Client())
	if err := sink.SaveFlowIngest(t.Context(), flowIngestPayload{RunID: "r-1"}); err == nil {
		t.Error("a push with an unreadable token succeeded; it must fail rather than send an empty bearer")
	}
	if reached {
		t.Error("the request reached the platform with no readable token")
	}
}
