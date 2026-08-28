package httpapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/store"
)

// fullRunBody 是一份每类资源都有一条记录的上报。
func fullRunBody() string {
	return `{"schemaVersion":1,"runId":"r-full","status":"OK",
	  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:05Z",
	  "observedAt":"2026-08-18T00:00:05Z",
	  "observation":{
	    "namespaces":[{"name":"shop","labels":{"team":"checkout"},
	                   "inMesh":true,"meshSource":"NAMESPACE_INJECTION",
	                   "meshDetail":"istio-injection=enabled"}],
	    "pods":[{"namespace":"shop","name":"web-1","uid":"u-1","phase":"Running",
	             "ip":"10.128.0.5","hostNetwork":true,"nodeName":"node-a",
	             "namedPorts":[{"name":"http","port":8080,"protocol":"TCP"}]}],
	    "nodes":[{"name":"node-a","podCidrs":["10.4.1.0/24"],"internalIps":["10.128.0.5"]}],
	    "services":[{"namespace":"shop","name":"web","type":"LoadBalancer",
	                 "selector":{"app":"web"},"clusterIp":"10.8.0.7",
	                 "ports":[{"name":"http","port":80,"targetPort":8080,"protocol":"TCP"}],
	                 "loadBalancerIngressIps":["203.0.113.9"],
	                 "loadBalancerSourceRanges":["10.0.0.0/8"]}],
	    "endpoints":[{"namespace":"shop","name":"web","addresses":["10.4.0.9"],"ports":[8080]}],
	    "policies":[{"namespace":"shop","name":"deny-all","uid":"p-1",
	                 "manifest":"apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\n"}],
	    "gateways":[{"namespace":"shop","name":"web-ing","kind":"Ingress","backendService":"web"}],
	    "warnings":[{"kind":"WORKLOAD_UNRESOLVED","subject":"shop/web-1","detail":"rs missing"}],
	    "foreignScopes":[{"namespace":"shop","matchLabels":{"app":"web"}}],
	    "foreignScopesComplete":true
	  },
	  "failures":[{"resource":"SERVICE","reason":"FORBIDDEN","detail":"services is forbidden"}]}`
}

// **这条用例存在的理由是一次真实集群演练。** agent 装进 kind 跑通之后，
// collection_run_resource 里 POD=30 而 NAMESPACE / SERVICE / NETWORKPOLICY /
// NODE 全是 0 —— 那些 0 是假话：agent 采到了，是报文只带 Pod 就丢掉了。
//
// 而按本项目的口径，写 0 的含义是「尝试了、集群里就是没有」。一个 push 集群
// 会显示成「没有任何 NetworkPolicy」，策略生成据此把它当作裸集群处理 ——
// 那是生产阻断方向的错误。
func TestIngestCarriesEveryResourceKindNotJustPods(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postRun(t, h, token, fullRunBody())
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: %s", got, rec.Body.String())
	}

	obs := sink.only(t).Observation
	counts := map[string]int{
		"namespaces": len(obs.Namespaces),
		"pods":       len(obs.Pods),
		"nodes":      len(obs.Nodes),
		"services":   len(obs.Services),
		"endpoints":  len(obs.Endpoints),
		"policies":   len(obs.Policies),
		"gateways":   len(obs.Gateways),
		"warnings":   len(obs.Warnings),
	}
	for kind, n := range counts {
		if n != 1 {
			t.Errorf("%s reached the sink %d times, want 1 — 这一类被报文丢掉了，"+
				"落库时会写成一个 0，而 0 的含义是「集群里就是没有」", kind, n)
		}
	}
}

func TestIngestKeepsTheFieldsTheEvaluationLayerReads(t *testing.T) {
	// 只数条数不够：一条丢了字段的记录照样计一条，而求值层读的是字段。
	// 这里逐个挑求值层真正依赖的那些。
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	if got := bodyOf(t, postRun(t, h, token, fullRunBody()))["code"]; got != float64(response.CodeOK) {
		t.Fatal("push failed")
	}
	obs := sink.only(t).Observation

	if got := obs.Namespaces[0].Labels["team"]; got != "checkout" {
		t.Errorf("namespace labels = %v — namespaceSelector 靠它匹配", obs.Namespaces[0].Labels)
	}
	if !obs.Namespaces[0].InMesh {
		t.Error("namespace InMesh was dropped — mesh 降级判定靠它")
	}
	if got := obs.Services[0].Selector["app"]; got != "web" {
		t.Errorf("service selector = %v — Baseline 取的是它而不是 ClusterIP", obs.Services[0].Selector)
	}
	if len(obs.Services[0].Ports) != 1 || obs.Services[0].Ports[0].TargetPort != 8080 {
		t.Errorf("service ports = %+v — 规则生成用的是 targetPort", obs.Services[0].Ports)
	}
	// 往返测试的接收侧一半：证明 LoadBalancer 暴露判定依据没有在解码这一步
	// 丢掉。发送侧那一半见 cmd/distill-agent 的 TestSinkEncodesTheExposureFields。
	if got := obs.Services[0].LoadBalancerIngressIPs; len(got) != 1 || got[0] != "203.0.113.9" {
		t.Errorf("service loadBalancerIngressIps = %v, want [203.0.113.9] — "+
			"这个 Service 在推送式接入下会永远显得没有 LB 入口", got)
	}
	if got := obs.Services[0].LoadBalancerSourceRanges; len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Errorf("service loadBalancerSourceRanges = %v, want [10.0.0.0/8]", got)
	}
	if got := obs.Pods[0].NamedPorts; len(got) != 1 || got[0] != (snapshot.NamedPort{Name: "http", Port: 8080, Protocol: "TCP"}) {
		t.Errorf("pod namedPorts = %+v, want one {http 8080 TCP} — "+
			"命名端口规则在求值层会解不出这个端口", got)
	}
	if len(obs.Endpoints[0].Addresses) != 1 {
		t.Errorf("endpoint addresses = %v — 一个没有后端的 Service 会生成指向空集的规则",
			obs.Endpoints[0].Addresses)
	}
	if !strings.Contains(obs.Policies[0].Manifest, "NetworkPolicy") {
		t.Errorf("policy manifest = %q — 那是「集群当时是什么样」的证据，"+
			"丢了就找不回来", obs.Policies[0].Manifest)
	}
	if obs.Gateways[0].BackendService != "web" {
		t.Errorf("gateway backend = %q — 它指出哪些 workload 是外部入口",
			obs.Gateways[0].BackendService)
	}
	if obs.Warnings[0].Kind != snapshot.WarningWorkloadUnresolved {
		t.Errorf("warning kind = %q — 告警条数与构成要进可见面", obs.Warnings[0].Kind)
	}
	// 往返测试的接收侧一半：第二平面的覆盖范围与完整度标志。发送侧那一半见
	// cmd/distill-agent 的 TestSinkEncodesTheForeignPlaneScopes。丢掉标志的
	// 后果是它恒为零值 —— 而零值即"范围不完整"，判定整片降级、每条观测被丢成
	// DEGRADED_EVIDENCE，这个集群一条学到的规则都产不出。
	if !obs.ForeignScopesComplete {
		t.Error("foreignScopesComplete 在解码这一步丢了 —— 平台会据此整片降级这个集群的判定")
	}
	if len(obs.ForeignScopes) != 1 ||
		obs.ForeignScopes[0].Namespace != "shop" ||
		obs.ForeignScopes[0].MatchLabels["app"] != "web" {
		t.Errorf("foreignScopes = %+v, want one {shop app=web} —— 降级收窄不到"+
			"真的被覆盖的那些主体", obs.ForeignScopes)
	}
	// 每一条记录的集群归属都由平台填，agent 说了不算。
	for _, got := range []string{
		obs.Namespaces[0].ClusterID, obs.Pods[0].ClusterID, obs.Nodes[0].ClusterID,
		obs.Services[0].ClusterID, obs.Endpoints[0].ClusterID,
		obs.Policies[0].ClusterID, obs.Gateways[0].ClusterID,
	} {
		if got != "prod-asia-1" {
			t.Errorf("a record carried ClusterID %q, want prod-asia-1", got)
		}
	}
}

// **采集失败必须过线。** 平台把 run.Failures 写进 collection_run_failure，
// 而 collectstore 的 `cameBack = enumerated && !failed` 读的正是那张表：
// 报文不带失败记录，那张表在推送模式下恒空、failed 恒为 false，于是
// "Service 列表被 403 拒了"被读成"我们看过了，这个集群就是没有 Service"。
// 顺着这条读法，DNS / LB_HEALTH_CHECK / EXPOSED_INGRESS / METRICS_SCRAPE
// 会被判成不适用、掉出 Missing()，Enforcing 门禁于是放行 —— 一次采集失败
// 变成一次放行（design review SC1，2026-08-28）。
func TestIngestCarriesTheCollectionFailures(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	if got := bodyOf(t, postRun(t, h, token, fullRunBody()))["code"]; got != float64(response.CodeOK) {
		t.Fatal("push failed")
	}
	run := sink.only(t)
	if len(run.Failures) != 1 {
		t.Fatalf("failures = %+v, want one record —— 一次被 403 拒掉的枚举会被"+
			"读成「集群里就是没有」，而那正好绕过 Enforcing 门禁", run.Failures)
	}
	f := run.Failures[0]
	if f.Resource != snapshot.ResourceService || f.Reason != snapshot.FailureForbidden {
		t.Errorf("failure = %+v, want {SERVICE FORBIDDEN}", f)
	}
	if f.Detail != "services is forbidden" {
		t.Errorf("failure detail = %q —— 排查 RBAC 的人只有这一句话能看", f.Detail)
	}
}

// 封闭枚举在边界上校验，不原样落库：collection_run_failure 的 resource 与
// reason 都只是 VARCHAR，封闭性只由 Go 侧保证（CLAUDE.md §3），而取值来自
// 别人集群里的进程。放一个不认识的字符串进去，统计口径上就再也看不出有
// 一类原因被漏登记了。
func TestIngestRejectsAFailureOutsideTheClosedEnums(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"an unknown resource kind", `{"schemaVersion":1,"runId":"r-bad-res","status":"PARTIAL",
		  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:05Z",
		  "observedAt":"2026-08-18T00:00:05Z","observation":{},
		  "failures":[{"resource":"CILIUMNETWORKPOLICY","reason":"FORBIDDEN"}]}`},
		{"an unknown reason", `{"schemaVersion":1,"runId":"r-bad-reason","status":"PARTIAL",
		  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:05Z",
		  "observedAt":"2026-08-18T00:00:05Z","observation":{},
		  "failures":[{"resource":"SERVICE","reason":"THROTTLED"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			h, _, cookie := newTestRouterWithAgentSink(t, sink)
			_, token := issueAgent(t, h, cookie, "prod-asia-1")

			rec := postRun(t, h, token, tc.body)
			if got := bodyOf(t, rec)["code"]; got == float64(response.CodeOK) {
				t.Fatalf("平台收下了一个不认识的失败取值: %s", rec.Body.String())
			}
			if len(sink.runs) != 0 {
				t.Error("被拒的上报仍然落了库")
			}
		})
	}
}

// 一个采过资产、没摄入过流量的集群，安全发现这一屏必须答得出裸奔 Pod。
//
// **API 层此前把它挡在了 Reader 之前**：没有 from/to 就去问默认窗口，而默认
// 窗口来自流量摄入。Reader 改成按资产作答之后，这一层不放行的话，页面上什么
// 都不会变（design doc 2026-08-18 §4.2）。
func TestSecurityEndpointAnswersWithoutAWindowWhenThereIsNoIngest(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, noIngestReader{}, reg)

	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/security")
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: %s", got, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"trafficObserved":false`) {
		t.Errorf("the report did not say traffic was never observed: %s", rec.Body.String())
	}
}

// noIngestReader 是一个「资产有、流量没有」的 Reader。
//
// DefaultWindow 答 ErrNoFlowIngest（真实 Reader 在这种集群上就是这么答的），
// Security 则照常给出裸奔 Pod 并标注没有观测。
type noIngestReader struct{ store.Reader }

func (noIngestReader) DefaultWindow(context.Context, string) (store.TimeWindow, error) {
	return store.TimeWindow{}, collectstore.ErrNoFlowIngest
}

func (noIngestReader) Security(_ context.Context, clusterID string, _ store.TimeWindow) (store.SecurityReport, error) {
	return store.SecurityReport{
		Cluster:         clusterID,
		TrafficObserved: false,
		NakedPods:       []store.NakedPod{{Cluster: clusterID, Namespace: "shop", Name: "web-1"}},
		RiskyFlows:      []store.RiskyFlow{},
		EgressTargets:   []store.EgressTarget{},
	}, nil
}
