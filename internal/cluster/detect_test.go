package cluster_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/cluster"
)

// 每一种已知 sidecar 都必须认出来。漏掉一种的后果是该 Pod 不被标记
// DEGRADED，于是一份失真的 L4 身份会被当成可信证据用来生成策略推荐 ——
// 错在"结果比现实好看"的方向，本平台必须往反方向错。
func TestDetectPodMeshRecognizesEveryKnownSidecar(t *testing.T) {
	cases := []struct {
		name       string
		containers []string
		wantSource cluster.MeshSource
	}{
		{"istio sidecar", []string{"app", "istio-proxy"}, cluster.MeshSourceIstioSidecar},
		{"linkerd sidecar", []string{"app", "linkerd-proxy"}, cluster.MeshSourceLinkerdSidecar},
		{"sidecar listed first", []string{"istio-proxy", "app"}, cluster.MeshSourceIstioSidecar},
		{"sidecar behind several containers", []string{"app", "logger", "linkerd-proxy"}, cluster.MeshSourceLinkerdSidecar},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cluster.DetectPodMesh(c.containers)
			if !got.InMesh {
				t.Fatalf("DetectPodMesh(%v).InMesh = false, want true", c.containers)
			}
			if got.Source != c.wantSource {
				t.Errorf("DetectPodMesh(%v).Source = %q, want %q", c.containers, got.Source, c.wantSource)
			}
			if got.Detail == "" {
				t.Errorf("DetectPodMesh(%v).Detail is empty, want the container name", c.containers)
			}
		})
	}
}

func TestDetectPodMeshReportsNoMeshWithoutASidecar(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"app"},
		{"app", "logger", "istio-proxy-init"},
		{"app", "linkerd-init"},
	}
	for _, containers := range cases {
		got := cluster.DetectPodMesh(containers)
		if got.InMesh {
			t.Errorf("DetectPodMesh(%v).InMesh = true, want false", containers)
		}
		if got.Source != "" || got.Detail != "" {
			t.Errorf("DetectPodMesh(%v) = %+v, want Source and Detail empty when not in mesh", containers, got)
		}
	}
}

func TestDetectNamespaceMeshReadsBothInjectionSwitches(t *testing.T) {
	cases := []struct {
		name       string
		labels     map[string]string
		wantSource cluster.MeshSource
		wantDetail string
	}{
		{
			name:       "injection label enabled",
			labels:     map[string]string{"istio-injection": "enabled"},
			wantSource: cluster.MeshSourceNamespaceInjection,
			wantDetail: "enabled",
		},
		{
			name:       "revision label",
			labels:     map[string]string{"istio.io/rev": "asm-1-20"},
			wantSource: cluster.MeshSourceNamespaceRevision,
			wantDetail: "asm-1-20",
		},
		{
			// 两个开关同时存在时报注入开关：它是显式的那一个。
			name:       "injection label wins over revision",
			labels:     map[string]string{"istio-injection": "enabled", "istio.io/rev": "asm-1-20"},
			wantSource: cluster.MeshSourceNamespaceInjection,
			wantDetail: "enabled",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cluster.DetectNamespaceMesh(c.labels)
			if !got.InMesh {
				t.Fatalf("DetectNamespaceMesh(%v).InMesh = false, want true", c.labels)
			}
			if got.Source != c.wantSource {
				t.Errorf("DetectNamespaceMesh(%v).Source = %q, want %q", c.labels, got.Source, c.wantSource)
			}
			if got.Detail != c.wantDetail {
				t.Errorf("DetectNamespaceMesh(%v).Detail = %q, want %q", c.labels, got.Detail, c.wantDetail)
			}
		})
	}
}

func TestDetectNamespaceMeshReportsNoMeshWithoutASwitch(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"no labels", nil},
		{"unrelated labels", map[string]string{"team": "payments"}},
		{"injection explicitly disabled", map[string]string{"istio-injection": "disabled"}},
		{"injection label present but empty", map[string]string{"istio-injection": ""}},
		{"revision label present but empty", map[string]string{"istio.io/rev": ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cluster.DetectNamespaceMesh(c.labels)
			if got.InMesh {
				t.Errorf("DetectNamespaceMesh(%v).InMesh = true, want false", c.labels)
			}
			if got.Source != "" || got.Detail != "" {
				t.Errorf("DetectNamespaceMesh(%v) = %+v, want Source and Detail empty", c.labels, got)
			}
		})
	}
}
