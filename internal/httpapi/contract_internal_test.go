package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// clusterWriteContractPath 是集群写入体的字段清单，前后端共读同一份。
const clusterWriteContractPath = "../../contracts/cluster-write.json"

type clusterWriteContract struct {
	Keys []string `json:"keys"`
}

// 集群写入体的字段集合必须与契约文件一致。
//
// 存在的理由是一次真实事故：clusterPayload 长出了 businessCycle*、
// managedSystemNamespaces* 与 enforcedPlanes*，前端 ClusterWrite 没跟上。
// PUT 是整体替换，于是**用界面编辑任何一个集群，都会把这六项静默清空** ——
// 一个带着理由做出的声明就此消失，而操作者下次打开页面看到的是「未声明」，
// 与从未声明过完全一样。
//
// 两侧各自有守卫却都是绿的：Go 那边测的是自己的行为，前端那条键集合断言
// 比的是前端自己的字面量。谁都没有在比**对方**。这份契约文件是那个缺失的
// 交汇点：加一个字段要动它，而动它会同时让另一侧红。
func TestClusterWritePayloadMatchesTheSharedContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(clusterWriteContractPath))
	if err != nil {
		t.Fatalf("read the shared cluster-write contract: %v", err)
	}
	var contract clusterWriteContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("decode the shared cluster-write contract: %v", err)
	}

	// 从 JSON 编码取键，而不是从结构体反射取字段名：写进线上的是 json tag，
	// 而字段名与 tag 完全可以不一样。前端看见的只有 tag。
	encoded, err := json.Marshal(clusterPayload{})
	if err != nil {
		t.Fatalf("marshal an empty clusterPayload: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode the encoded clusterPayload: %v", err)
	}

	got := make([]string, 0, len(fields))
	for k := range fields {
		got = append(got, k)
	}
	slices.Sort(got)
	want := slices.Clone(contract.Keys)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("clusterPayload 的字段集合与 %s 不一致。\n"+
			"服务端: %v\n契约:   %v\n"+
			"加字段要三处一起动：clusterPayload、契约文件、前端 ClusterWrite。"+
			"只动前两处，界面提交时会把新字段清空。",
			clusterWriteContractPath, got, want)
	}
}
