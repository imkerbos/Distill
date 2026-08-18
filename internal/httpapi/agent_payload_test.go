package httpapi_test

import (
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshot"
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
	             "ip":"10.128.0.5","hostNetwork":true,"nodeName":"node-a"}],
	    "nodes":[{"name":"node-a","podCidrs":["10.4.1.0/24"],"internalIps":["10.128.0.5"]}],
	    "services":[{"namespace":"shop","name":"web","type":"ClusterIP",
	                 "selector":{"app":"web"},"clusterIp":"10.8.0.7",
	                 "ports":[{"name":"http","port":80,"targetPort":8080,"protocol":"TCP"}]}],
	    "endpoints":[{"namespace":"shop","name":"web","addresses":["10.4.0.9"],"ports":[8080]}],
	    "policies":[{"namespace":"shop","name":"deny-all","uid":"p-1",
	                 "manifest":"apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\n"}],
	    "gateways":[{"namespace":"shop","name":"web-ing","kind":"Ingress","backendService":"web"}],
	    "warnings":[{"kind":"WORKLOAD_UNRESOLVED","subject":"shop/web-1","detail":"rs missing"}]
	  }}`
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
