package main

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// testDynamic 是一个空的动态客户端：这些测试的集群装了 ANP 一族的 CRD，
// 但一条对象都没有。
//
// 传 nil 会让每一轮多出一条"采不了管理面策略"的失败，把这些测试真正要看的
// 那件事埋在噪声里 —— 而那条失败本身有它自己的用例（internal/collect）。
func testDynamic() *dynamicfake.FakeDynamicClient {
	gvr := func(r string) schema.GroupVersionResource {
		return schema.GroupVersionResource{
			Group: "policy.networking.k8s.io", Version: "v1alpha1", Resource: r}
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), map[schema.GroupVersionResource]string{
			gvr("adminnetworkpolicies"):         "AdminNetworkPolicyList",
			gvr("baselineadminnetworkpolicies"): "BaselineAdminNetworkPolicyList",
		})
}
