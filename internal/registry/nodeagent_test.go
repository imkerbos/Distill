package registry_test

import (
	"errors"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshot"
)

func validNodeAgent() registry.NodeAgentRegistration {
	return registry.NodeAgentRegistration{
		Namespace: "logging", App: "filebeat", HostNetwork: true, TargetPort: 9200,
	}
}

func TestValidateNodeAgentRequiresEverySelectorHalf(t *testing.T) {
	if err := registry.ValidateNodeAgent(validNodeAgent()); err != nil {
		t.Fatalf("ValidateNodeAgent(valid) = %v, want nil", err)
	}
	for name, mutate := range map[string]func(*registry.NodeAgentRegistration){
		"no namespace": func(a *registry.NodeAgentRegistration) { a.Namespace = "" },
		"no app":       func(a *registry.NodeAgentRegistration) { a.App = "" },
		// **端口不许留空、也不许猜一个默认值。** 一条放行到猜出来的端口的
		// 规则，看起来齐备、实际什么都没放行，而症状要到监控中断时才出现。
		"no port":       func(a *registry.NodeAgentRegistration) { a.TargetPort = 0 },
		"port too high": func(a *registry.NodeAgentRegistration) { a.TargetPort = 70000 },
	} {
		t.Run(name, func(t *testing.T) {
			a := validNodeAgent()
			mutate(&a)
			err := registry.ValidateNodeAgent(a)
			if err == nil {
				t.Fatalf("ValidateNodeAgent(%s) = nil, want an error", name)
			}
			if !errors.Is(err, registry.ErrInvalid) {
				t.Errorf("error = %v, want ErrInvalid", err)
			}
		})
	}
}

// 同时登记 agent 又声明「不适用」是自相矛盾的登记。
//
// 收下它之后没有任何一屏知道该信哪一半：缺失清单该不该有这一类？
// 拒绝在写入之前，好过让两个互相矛盾的事实并存。
func TestAClusterCannotBothRegisterAgentsAndDeclareThemInapplicable(t *testing.T) {
	c := registry.Cluster{
		ID: "c", DisplayName: "C", PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		State:              registry.StateRegistered,
		NodeAgents:         []registry.NodeAgentRegistration{validNodeAgent()},
		NoNodeAgentsReason: "本集群的 agent 都只读文件",
	}
	if err := registry.ValidateCluster(c); err == nil {
		t.Error("ValidateCluster(both) = nil — 一次自相矛盾的登记被收下了")
	}
}

func TestNodeAgentSnapshotsCarryWhatDerivationNeeds(t *testing.T) {
	c := registry.Cluster{ID: "prod-asia-1", NodeAgents: []registry.NodeAgentRegistration{
		{Namespace: "logging", App: "filebeat", HostNetwork: true, TargetPort: 9200},
		{Namespace: "obs", App: "vector", HostNetwork: false, TargetPort: 8686},
	}}
	got := c.NodeAgentSnapshots()
	if len(got) != 2 {
		t.Fatalf("NodeAgentSnapshots() = %+v, want 2", got)
	}
	want := snapshot.NodeAgent{
		ClusterID: "prod-asia-1", Namespace: "logging", App: "filebeat",
		HostNetwork: true, TargetPort: 9200,
	}
	if got[0] != want {
		t.Errorf("NodeAgentSnapshots()[0] = %+v, want %+v", got[0], want)
	}
	// hostNetwork 必须原样传下去：为 true 时推导要走 node CIDR，写成
	// podSelector 会得到一条看起来正确、实际从不匹配的规则。
	if got[1].HostNetwork {
		t.Errorf("a non-hostNetwork agent was marked hostNetwork: %+v", got[1])
	}
}

func TestNodeAgentsAreEmptyWithoutRegistration(t *testing.T) {
	c := registry.Cluster{ID: "c"}
	if got := c.NodeAgentSnapshots(); len(got) != 0 {
		t.Errorf("NodeAgentSnapshots() = %+v, want none", got)
	}
}
