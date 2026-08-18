package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// captured 记下服务端收到的一次请求。
type captured struct {
	path   string
	auth   string
	body   map[string]any
	rawGot string
}

// stubPlatform 是一个只记录请求、按需作答的平台替身。
func stubPlatform(t *testing.T, status int, reply string, got *captured) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.rawGot = string(raw)
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
}

const okReply = `{"code":0,"msg":"ok","data":{"runId":"r-1"}}`

func sampleRunForPush() snapshot.Run {
	return snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 8, 18, 0, 0, 5, 0, time.UTC),
		Observation: snapshot.Observation{
			ClusterID:  "whatever-the-agent-thinks",
			RunID:      "r-1",
			ObservedAt: time.Date(2026, 8, 18, 0, 0, 5, 0, time.UTC),
			Pods: []snapshot.Pod{{
				Namespace: "app", Name: "web-1", UID: "u-1",
				Phase: "Running", IP: "10.4.0.9",
			}},
		},
	}
}

func TestSinkSendsTheTokenAsABearerHeader(t *testing.T) {
	var got captured
	srv := stubPlatform(t, http.StatusOK, okReply, &got)
	defer srv.Close()

	sink := newHTTPSink(srv.URL, "dstl_0011223344556677_c2VjcmV0")
	if err := sink.Save(context.Background(), sampleRunForPush()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got.auth != "Bearer dstl_0011223344556677_c2VjcmV0" {
		t.Errorf("Authorization = %q, want the bearer token", got.auth)
	}
	if got.path != "/api/v1/agent/collection-runs" {
		t.Errorf("path = %q, want the ingest endpoint", got.path)
	}
}

func TestSinkNeverClaimsACluster(t *testing.T) {
	var got captured
	srv := stubPlatform(t, http.StatusOK, okReply, &got)
	defer srv.Close()

	// 集群归属只能来自 token（design doc 2026-08-18 §2）。报文里带上它会被
	// 服务端**整体拒绝** —— 两边都不给「自己声明归属」留路，因此这条断言
	// 与服务端那条不是重复，而是同一条不变式的两半。
	if err := newHTTPSink(srv.URL, "dstl_x_y").Save(context.Background(), sampleRunForPush()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, banned := range []string{"clusterId", "clusterID", "whatever-the-agent-thinks"} {
		if strings.Contains(got.rawGot, banned) {
			t.Errorf("the pushed payload carried %q: %s", banned, got.rawGot)
		}
	}
}

func TestSinkNeverClaimsAScope(t *testing.T) {
	var got captured
	srv := stubPlatform(t, http.StatusOK, okReply, &got)
	defer srv.Close()

	// 归属判定是平台的事。agent 侧连发出去的语法都不该有 —— 发了会被
	// 服务端按未知字段拒掉，而那是一次本可以在这一侧避免的失败。
	run := sampleRunForPush()
	run.Observation.Pods[0].IPScope = "POD"
	if err := newHTTPSink(srv.URL, "dstl_x_y").Save(context.Background(), run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if strings.Contains(got.rawGot, "ipScope") || strings.Contains(got.rawGot, "POD") {
		t.Errorf("the pushed payload carried a scope: %s", got.rawGot)
	}
}

func TestSinkDeclaresTheSchemaVersion(t *testing.T) {
	var got captured
	srv := stubPlatform(t, http.StatusOK, okReply, &got)
	defer srv.Close()

	if err := newHTTPSink(srv.URL, "dstl_x_y").Save(context.Background(), sampleRunForPush()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// 不声明版本，服务端会按「不认识」拒绝。这一条钉住的是两侧对同一个
	// 常量达成一致，而不是各写各的。
	if got.body["schemaVersion"] != float64(1) {
		t.Errorf("schemaVersion = %v, want 1: %s", got.body["schemaVersion"], got.rawGot)
	}
}

func TestSinkFailsOnARefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reply  string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"code":10005,"msg":"nope"}`},
		{"business failure", http.StatusOK, `{"code":20001,"msg":"bad payload"}`},
		{"server error", http.StatusInternalServerError, `{"code":50001,"msg":"boom"}`},
		{"garbage", http.StatusOK, `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got captured
			srv := stubPlatform(t, tc.status, tc.reply, &got)
			defer srv.Close()

			// 被拒的上报必须是一次失败。静默成功会让一个 token 早就被吊销
			// 的 agent 看起来一直在正常工作，而那个集群的数据停在被吊销
			// 的那一刻 —— 页面上没有任何症状。
			if err := newHTTPSink(srv.URL, "dstl_x_y").Save(context.Background(),
				sampleRunForPush()); err == nil {
				t.Error("Save() = nil, want an error")
			}
		})
	}
}

func TestSinkErrorNeverEchoesTheToken(t *testing.T) {
	var got captured
	srv := stubPlatform(t, http.StatusUnauthorized, `{"code":10005}`, &got)
	defer srv.Close()

	// 拼出来而不是写成一个字面量：一个长得像凭据的常量会被扫描器与
	// gosec 一起点名，而这里要的恰恰是「一把看起来很真的 token」。
	const token = "dstl_" + "0011223344556677" + "_" + "c2VjcmV0LXZhbHVl"
	err := newHTTPSink(srv.URL, token).Save(context.Background(), sampleRunForPush())
	if err == nil {
		t.Fatal("Save() = nil, want an error")
	}
	// 这个进程的日志终点是集群日志。错误里带出 token，等于把凭据写进
	// 一处谁都读得到的地方（规范 §19、§21）。
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Errorf("the error carried the token: %v", err)
	}
}

func TestSinkReportsAnAbortedRunSoItIsNotMistakenForNeverCollected(t *testing.T) {
	var got captured
	srv := stubPlatform(t, http.StatusOK, okReply, &got)
	defer srv.Close()

	// 一次在读到集群之前就失败的运行必须留下痕迹。不上报的话，界面显示
	// 「这个集群还没有过任何一次资产采集」—— 与一个 agent 压根没被拉起来
	// 的集群一模一样，操作者会去等一次永远不会成功的采集。
	err := newHTTPSink(srv.URL, "dstl_x_y").SaveAbortedRun(context.Background(),
		"ignored-cluster", "r-2",
		time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 18, 0, 0, 1, 0, time.UTC),
		snapshot.RunErrorCredentialUnavailable)
	if err != nil {
		t.Fatalf("SaveAbortedRun() error = %v", err)
	}
	if got.body["status"] != string(snapshot.RunFailed) {
		t.Errorf("status = %v, want %q", got.body["status"], snapshot.RunFailed)
	}
	if got.body["errorReason"] != string(snapshot.RunErrorCredentialUnavailable) {
		t.Errorf("errorReason = %v, want %q — 没有原因的失败运行，在界面上与"+
			"一次采到零资源的成功运行无法区分",
			got.body["errorReason"], snapshot.RunErrorCredentialUnavailable)
	}
	if strings.Contains(got.rawGot, "ignored-cluster") {
		t.Errorf("the aborted run claimed a cluster: %s", got.rawGot)
	}
}

// httpSink 必须满足采集的落库口，否则 push 模式装配不出来。
//
// 只到 collectrun.Store 为止：流量摄入与身份推导在推送式接入下都不归
// agent —— 前者本轮没有来源，后者要读整张区间表，那是平台侧的事。多装两个
// 接口会逼出两个假实现，而假实现会让「这条路还没接通」看起来像「已经接通了」。
var _ collectrun.Store = (*httpSink)(nil)

// --- 参数校验 ---

func TestDispatchRejectsANonPositiveTimeout(t *testing.T) {
	// 非正超时不是「不限时」，是一个写错了的配置：一次没人能取消的采集
	// 会一直占着这个 Pod，而下一次 CronJob 又会拉起一个新的。
	err := dispatch(options{platformURL: "https://p.example", tokenFile: "/run/token"}, 0)
	if err == nil {
		t.Error("dispatch(timeout 0) = nil, want an error")
	}
}

func TestOptionsValidation(t *testing.T) {
	cases := []struct {
		name string
		opts options
	}{
		{"no platform url", options{tokenFile: "/run/token"}},
		{"no token file", options{platformURL: "https://platform.example"}},
		// 地址必须是 http(s)：一个写成 "platform.example" 的地址会在拼出
		// 请求时才失败，那时这一轮已经跑起来了。
		{"a scheme-less url", options{platformURL: "platform.example", tokenFile: "/run/token"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.validate(); err == nil {
				t.Errorf("validate(%s) = nil, want an error", tc.name)
			}
		})
	}
}

func TestReadTokenRejectsAnEmptyFileAndNeverEchoesThePath(t *testing.T) {
	dir := t.TempDir()
	empty := dir + "/empty"
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readToken(empty); err == nil {
		t.Error("readToken(empty file) = nil — 一把空 token 会让每一次上报都被拒，" +
			"而失败点在最后一步，离成因很远")
	}
	// 路径是部署布局信息，不进错误文本（规范 §19、§22）。
	_, err := readToken(dir + "/missing")
	if err == nil {
		t.Fatal("readToken(missing) = nil, want an error")
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error carried the path: %v", err)
	}
}

func TestReadTokenTrimsWhatTheSecretMountAdds(t *testing.T) {
	// 挂载的 Secret 常带一个结尾换行。不去掉的话，Authorization 头里会多
	// 一个字节，而平台那侧的答复是「这把 token 不认识」—— 一条指向完全
	// 错误方向的线索。
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte("dstl_0011223344556677_c2VjcmV0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readToken(path)
	if err != nil {
		t.Fatalf("readToken() error = %v", err)
	}
	if got != "dstl_0011223344556677_c2VjcmV0" {
		t.Errorf("readToken() = %q, want it trimmed", got)
	}
}

// 推送必须带上每一类资源，不只是 Pod。
//
// **这条用例来自一次真集群演练**：agent 装进 kind 跑通之后，
// collection_run_resource 里 POD=30 而 NAMESPACE / SERVICE / NETWORKPOLICY /
// NODE 全是 0 —— 那些 0 是假话，agent 采到了，是报文丢掉了。而按本项目的
// 口径，写 0 的含义是「尝试了、集群里就是没有」：一个 push 集群会显示成
// 「没有任何 NetworkPolicy」，策略生成据此把它当作裸集群处理。
func TestSinkPushesEveryResourceKind(t *testing.T) {
	var got captured
	srv := stubPlatform(t, http.StatusOK, okReply, &got)
	defer srv.Close()

	run := sampleRunForPush()
	run.Observation.Namespaces = []snapshot.Namespace{{Name: "shop", Labels: map[string]string{"team": "x"}}}
	run.Observation.Nodes = []snapshot.Node{{Name: "node-a", InternalIPs: []string{"10.128.0.5"}}}
	run.Observation.Services = []snapshot.Service{{
		Namespace: "shop", Name: "web", Type: "ClusterIP",
		Selector: map[string]string{"app": "web"},
		Ports:    []snapshot.ServicePort{{Name: "http", Port: 80, TargetPort: 8080, Protocol: "TCP"}},
	}}
	run.Observation.Endpoints = []snapshot.Endpoints{{Namespace: "shop", Name: "web", Addresses: []string{"10.4.0.9"}}}
	run.Observation.Policies = []snapshot.NetworkPolicy{{Namespace: "shop", Name: "deny", UID: "p-1", Manifest: "kind: NetworkPolicy"}}
	run.Observation.Gateways = []snapshot.Gateway{{Namespace: "shop", Name: "ing", Kind: "Ingress", BackendService: "web"}}
	run.Observation.Warnings = []snapshot.Warning{{Kind: snapshot.WarningWorkloadUnresolved, Subject: "shop/web-1"}}

	if err := newHTTPSink(srv.URL, "dstl_x_y").Save(context.Background(), run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	obs, ok := got.body["observation"].(map[string]any)
	if !ok {
		t.Fatalf("the payload has no observation: %s", got.rawGot)
	}
	for _, kind := range []string{
		"namespaces", "pods", "nodes", "services", "endpoints", "policies", "gateways", "warnings",
	} {
		list, ok := obs[kind].([]any)
		if !ok || len(list) != 1 {
			t.Errorf("%s in the payload = %v, want one record — 这一类被丢掉了，"+
				"平台落库时会写成一个 0，而 0 的含义是「集群里就是没有」", kind, obs[kind])
		}
	}
	// 每一类都不得自称集群：归属只来自 token，带上会被平台整体拒。
	if strings.Contains(got.rawGot, "clusterId") || strings.Contains(got.rawGot, "ClusterID") {
		t.Errorf("the payload claimed a cluster: %s", got.rawGot)
	}
}

func TestSinkTranslatesTheCodesAnOperatorCanActOn(t *testing.T) {
	// 这个进程的日志是运维在被管集群里唯一看得到的东西。「code 20008」
	// 需要他去查平台才知道是什么意思，而那时他手上只有一个失败的 Pod。
	cases := []struct {
		name string
		code int
		want string
	}{
		{"a concurrent collection", int(response.CodeConcurrentCollection), "only one collector"},
		{"a revoked token", int(response.CodeAgentUnauthenticated), "revoked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got captured
			srv := stubPlatform(t, http.StatusOK,
				`{"code":`+itoa(tc.code)+`,"msg":"平台写的文案"}`, &got)
			defer srv.Close()

			err := newHTTPSink(srv.URL, "dstl_x_y").Save(context.Background(), sampleRunForPush())
			if err == nil {
				t.Fatal("Save() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// 平台写的 msg 不转发：这一侧只说自己写的句子。
			if strings.Contains(err.Error(), "平台写的文案") {
				t.Errorf("the error forwarded the platform's message: %v", err)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
