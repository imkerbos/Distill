package kubeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// 探测失败必须报「没查成」，而不是「没有」（design doc 2026-08-25 §2.3）。
//
// 这是本文件唯一真正重要的那条性质：一次查不动的探测，如果在下游读成
// "确认不存在"，平台就会以满置信度回答每一条判定 —— 而集群里可能正跑着
// 一份优先级更高的 AdminNetworkPolicy 在覆盖那些结论。
func TestAProbeThatCannotDiscoverIsNotAProbeThatFoundNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	got, err := probePlanes(context.Background(), &rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("probePlanes() = %v", err)
	}
	if got.Checked {
		t.Error("发现接口整个不可用，探测却报「查过了」")
	}
	if got.Present {
		t.Error("什么都没查到，却报「存在」")
	}
}

// API 组存在、但列不出对象（没有 RBAC）时，同样是「没查成」。
//
// 不能拿其余几类的「没有」凑成一个整体的「没有」：只要有一类答不出来，
// 整次探测的结论就只能是"我不知道"。
func TestAPlaneThatCannotBeListedDegradesTheWholeProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api", "/apis":
			writeJSON(t, w, metav1.APIGroupList{Groups: []metav1.APIGroup{{
				Name: "cilium.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "cilium.io/v2", Version: "v2"},
				},
			}}})
		default:
			// 组在、列不出来。
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	}))
	defer srv.Close()

	got, err := probePlanes(context.Background(), &rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("probePlanes() = %v", err)
	}
	if got.Checked {
		t.Error("有一类列不出来，整次探测却报「查过了」——"+
			"那会让下游把它读成「确认不存在」", got)
	}
}

// 组不存在 = 一个确定的「没有」。这一条是对照组：没有它，一个恒返回
// Checked=false 的实现也能让上面两条通过，而那等于把探测整个关掉。
func TestAClusterWithoutThosePlanesIsAConfirmedNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, metav1.APIGroupList{Groups: []metav1.APIGroup{}})
	}))
	defer srv.Close()

	got, err := probePlanes(context.Background(), &rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("probePlanes() = %v", err)
	}
	if !got.Checked {
		t.Error("集群里根本没有那几个 API 组，探测却报「没查成」")
	}
	if got.Present {
		t.Error("报了「存在」，而集群里连那几个 API 组都没有")
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
}
