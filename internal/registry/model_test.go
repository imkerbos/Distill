package registry_test

import (
	"reflect"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// 三个接入状态的字面值必须钉死。
//
// 它们既是 onboard_state 的列值（迁移脚本、种子数据、既有行都写着这三个
// 字符串），也是 JSON 契约里前端 ONBOARD_STATE_LABEL 的键。改一个字母
// 不会有编译错误，也不会有任何测试失败 —— 表现是老行读不出来、界面显示
// 空白，而这两处都不会报错。
func TestOnboardStateLiteralsArePinned(t *testing.T) {
	for got, want := range map[registry.OnboardState]string{
		registry.StateRegistered: "REGISTERED",
		registry.StateObserving:  "OBSERVING",
		registry.StateReady:      "READY",
	} {
		if string(got) != want {
			t.Errorf("OnboardState literal = %q, want %q", got, want)
		}
	}
}

func TestOnboardStateEnumIsClosed(t *testing.T) {
	states := registry.AllOnboardStates()
	if len(states) != 3 {
		t.Fatalf("AllOnboardStates() = %d entries, want 3", len(states))
	}
	for _, s := range states {
		if !s.Valid() {
			t.Errorf("registered state %q reported invalid", s)
		}
	}
	if registry.OnboardState("ENFORCED").Valid() {
		t.Error("unregistered state reported valid")
	}
}

func TestImportRoleEnumIsClosed(t *testing.T) {
	roles := registry.AllImportRoles()
	if len(roles) != 2 {
		t.Fatalf("AllImportRoles() = %d entries, want 2", len(roles))
	}
	for _, r := range roles {
		if !r.Valid() {
			t.Errorf("registered role %q reported invalid", r)
		}
	}
	if registry.ImportRole("CURRENT").Valid() {
		t.Error("unregistered role reported valid")
	}
}

func TestImportSourceEnumIsClosed(t *testing.T) {
	sources := registry.AllImportSources()
	if len(sources) != 3 {
		t.Fatalf("AllImportSources() = %d entries, want 3", len(sources))
	}
	for _, s := range sources {
		if !s.Valid() {
			t.Errorf("registered source %q reported invalid", s)
		}
	}
	if registry.ImportSource("UPLOAD").Valid() {
		t.Error("unregistered source reported valid")
	}
}

// ToSnapshot 是 registry 与既有 Baseline 推导之间的唯一桥梁。
// 字段漏映射不会有编译错误，只会让某类 Baseline 悄悄推导不出来。
//
// 整体比对而非挑字段断言：审阅时的实证是，把 ToSnapshot 里那句防御性的
// copy(sources, ...) 删掉（只留 make），整个仓库依旧全绿 —— 健康检查
// 网段会变成一串空字符串，而空网段推不出任何 Baseline 规则，症状是
// 少一条放行、不是报错。
func TestToSnapshotCarriesEveryDerivationSource(t *testing.T) {
	c := registry.Cluster{
		ID: "c1", PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
		HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
	}
	want := snapshot.ClusterRegistry{
		ClusterID: "c1", PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
	}
	got := c.ToSnapshot()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToSnapshot() =\n%+v\nwant\n%+v", got, want)
	}
}

// 快照持有的必须是一份拷贝：推导层拿到的切片若与注册信息共享底层数组，
// 一次下游改写会悄悄改掉注册表里的网段，而两边都不会报错。
func TestToSnapshotDoesNotAliasTheRegisteredSlice(t *testing.T) {
	c := registry.Cluster{
		ID: "c1", PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		HealthCheckSources: []string{"35.191.0.0/16"},
	}
	got := c.ToSnapshot()
	got.HealthCheckSources[0] = "0.0.0.0/0"
	if c.HealthCheckSources[0] != "35.191.0.0/16" {
		t.Errorf("registered HealthCheckSources[0] = %q, want it untouched — "+
			"the snapshot aliases the registry's slice", c.HealthCheckSources[0])
	}
}

// APIServerSnapshots 与 ToSnapshot 同理：control-plane Baseline 取的是
// 这份清单，漏一个端点就是漏一条放行规则。
func TestAPIServerSnapshotsCarryEveryEndpoint(t *testing.T) {
	c := registry.Cluster{
		ID: "c1",
		APIServers: []registry.APIServer{
			{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
			{Host: "10.9.0.3", CIDR: "10.9.0.0/28", Port: 6443},
		},
	}
	want := []snapshot.APIServerEndpoint{
		{ClusterID: "c1", Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
		{ClusterID: "c1", Host: "10.9.0.3", CIDR: "10.9.0.0/28", Port: 6443},
	}
	if got := c.APIServerSnapshots(); !reflect.DeepEqual(got, want) {
		t.Errorf("APIServerSnapshots() =\n%+v\nwant\n%+v", got, want)
	}
}
