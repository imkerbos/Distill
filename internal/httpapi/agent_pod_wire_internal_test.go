package httpapi

import (
	"testing"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/collect"
)

// 五个字段过线之后要**落到 snapshot.Pod 上**。反射守卫钉的是"toRun 读过
// 它们"，读过不等于放对了地方——把 InMesh 抄进 MeshDetail 一样能过守卫。
func TestToRunLandsExtraAddressesMeshAndScrapeAnnotations(t *testing.T) {
	p := agentRunPayload{Observation: agentObservationPayload{
		Pods: []agentPodPayload{{
			Namespace:         "shop",
			Name:              "api-0",
			IP:                "10.0.0.5",
			ExtraIPs:          []string{"fd00::5", "fd00::6"},
			InMesh:            true,
			MeshSource:        string(cluster.MeshSourceIstioSidecar),
			MeshDetail:        "istio-proxy",
			ScrapeAnnotations: map[string]string{"prometheus.io/scrape": "true"},
		}},
	}}

	got := p.toRun("c1").Observation.Pods[0]

	if len(got.ExtraIPs) != 2 || got.ExtraIPs[0].IP != "fd00::5" || got.ExtraIPs[1].IP != "fd00::6" {
		t.Fatalf("ExtraIPs = %+v, want fd00::5 与 fd00::6", got.ExtraIPs)
	}
	// **归属留空**：它是平台的判定，由 collect.Classify 在下一步算。
	// toRun 自己填一个，等于让报文里根本没有的东西凭空出现。
	for _, a := range got.ExtraIPs {
		if a.Scope != "" || a.Reason != "" {
			t.Errorf("toRun 给第二地址 %s 填了归属 %q/%q —— 那是 Classify 的活",
				a.IP, a.Scope, a.Reason)
		}
	}
	if !got.InMesh {
		t.Error("InMesh 没落上 —— 求值引擎据此降级，恒为 false 是朝放宽的方向错")
	}
	if got.MeshSource != cluster.MeshSourceIstioSidecar {
		t.Errorf("MeshSource = %q, want %q", got.MeshSource, cluster.MeshSourceIstioSidecar)
	}
	if got.MeshDetail != "istio-proxy" {
		t.Errorf("MeshDetail = %q, want istio-proxy", got.MeshDetail)
	}
	if got.ScrapeAnnotations["prometheus.io/scrape"] != "true" {
		t.Errorf("ScrapeAnnotations = %v", got.ScrapeAnnotations)
	}
}

// 收下之后归属由平台算出来。这是 ExtraIPs 只发地址那个决定的另一半：
// 不发归属**且**收下时补上，缺哪一半都不成立。
func TestClassifyFillsTheScopeOfEveryPushedExtraAddress(t *testing.T) {
	p := agentRunPayload{Observation: agentObservationPayload{
		Pods: []agentPodPayload{{
			Namespace: "shop", Name: "api-0",
			IP:       "10.0.0.5",
			ExtraIPs: []string{"fd00::5"},
		}},
	}}

	got := collect.Classify(p.toRun("c1"), cluster.NewRegistry(nil)).Observation.Pods[0]

	if len(got.ExtraIPs) != 1 {
		t.Fatalf("ExtraIPs = %+v", got.ExtraIPs)
	}
	if got.ExtraIPs[0].Scope == "" {
		t.Errorf("第二地址收下之后仍然没有归属 —— 它会一路空到求值层:\n%+v", got.ExtraIPs[0])
	}
}
