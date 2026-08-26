package httpapi

import (
	"reflect"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

// clusterOf 给出一个零值集群，供反射取字段清单。
func clusterOf() registry.Cluster { return registry.Cluster{} }

// notWritableViaAPI 是 registry.Cluster 上**刻意不接受调用方写入**的字段。
//
// 每一项都要有理由，而不是"暂时没接"：
var notWritableViaAPI = map[string]string{
	"State":       "接入状态反映平台实际收到了什么，不是调用方的意愿（toCluster 明确忽略它）",
	"DataSource":  "让调用方指定来源等于给出一条把真集群标成演示集群的入口",
	"Git":         "绑定走 BIND_GIT_REPO，集群写路径不碰它，否则会多一条绕开审计的路",
	"OtherPlanes": "探测结果，由采集写；人工只能经 SetOtherPlanes 降级",
	"CNI":         "采集从 kube-system 的 Pod 认出来的事实，不是可填的字段",
}

// **registry.Cluster 上每一个可写字段都必须出现在请求体里。**
//
// 这条用例防的是一整类错误：往 Cluster 上加一个字段、写好落库与校验、
// 却忘了接进 HTTP 层。那时后端测试全绿，而字段在 API 上**静默消失** ——
// 调用方发的值被忽略，校验也就永远不会被触发。
//
// 真发生过两次：ManagedSystemNamespaces 与 EnforcedPlanes 都是落库、校验、
// 单测齐备之后，在真集群上 PUT 才发现根本写不进去（2026-08-26）。
//
// 加字段时要么给 clusterPayload 加上对应项，要么把它登记进
// notWritableViaAPI 并写下为什么 —— 两者都是一次明示的决定。
func TestClusterPayloadCoversEveryWritableField(t *testing.T) {
	payloadFields := map[string]bool{}
	pt := reflect.TypeOf(clusterPayload{})
	for i := range pt.NumField() {
		payloadFields[pt.Field(i).Name] = true
	}
	// 请求体里这几项与领域字段不同名，单独对上。
	payloadFields["BusinessCycle"] = payloadFields["BusinessCycleSeconds"]

	ct := reflect.TypeOf(clusterOf())
	var missing []string
	for i := range ct.NumField() {
		name := ct.Field(i).Name
		if _, exempt := notWritableViaAPI[name]; exempt {
			continue
		}
		if !payloadFields[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("registry.Cluster 的这些字段没有出现在 clusterPayload 里：%v\n"+
			"它们在 API 上会被静默忽略 —— 调用方发的值丢掉，校验也永远不会触发。\n"+
			"要么给 clusterPayload 加上，要么登记进 notWritableViaAPI 并写下理由。", missing)
	}
}

// 豁免清单本身不得腐烂：登记了一个已经不存在的字段，说明清单没跟上重构，
// 而那时它对真正新增的字段也就不再有约束力。
func TestExemptionListHasNoStaleEntries(t *testing.T) {
	ct := reflect.TypeOf(clusterOf())
	for name := range notWritableViaAPI {
		if _, ok := ct.FieldByName(name); !ok {
			t.Errorf("豁免清单里的 %q 已经不是 registry.Cluster 的字段了", name)
		}
	}
}
