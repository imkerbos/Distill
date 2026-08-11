package registry_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

func validCluster() registry.Cluster {
	return registry.Cluster{
		ID: "prod-asia-1", DisplayName: "Asia Prod",
		PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		State:              registry.StateRegistered,
		APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
		HealthCheckSources: []string{"35.191.0.0/16"},
	}
}

func TestValidateClusterAcceptsAWellFormedCluster(t *testing.T) {
	if err := registry.ValidateCluster(validCluster()); err != nil {
		t.Errorf("ValidateCluster() error = %v, want nil", err)
	}
}

func TestValidateClusterRejectsEmptyID(t *testing.T) {
	c := validCluster()
	c.ID = ""
	err := registry.ValidateCluster(c)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// 网段写错会让 Baseline 生成一条永远匹配不上的规则，而它外观完全正常。
// 校验必须在入库前拦住，不能等到推导时才发现。
func TestValidateClusterRejectsMalformedCIDR(t *testing.T) {
	for name, mutate := range map[string]func(*registry.Cluster){
		"podCIDR":     func(c *registry.Cluster) { c.PodCIDR = "10.4.0/14" },
		"nodeCIDR":    func(c *registry.Cluster) { c.NodeCIDR = "not-a-cidr" },
		"apiserver":   func(c *registry.Cluster) { c.APIServers[0].CIDR = "10.9.0.0/99" },
		"healthCheck": func(c *registry.Cluster) { c.HealthCheckSources[0] = "35.191.0.0" },
	} {
		t.Run(name, func(t *testing.T) {
			c := validCluster()
			mutate(&c)
			err := registry.ValidateCluster(c)
			if !errors.Is(err, registry.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			if err != nil && !strings.Contains(err.Error(), name) &&
				!strings.Contains(err.Error(), "cidr") {
				t.Errorf("err = %q, want it to name the offending field", err)
			}
		})
	}
}

func TestValidateClusterRejectsUnregisteredState(t *testing.T) {
	c := validCluster()
	c.State = "ENFORCED"
	if err := registry.ValidateCluster(c); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// Git 绑定要么完整，要么不填。填一半的绑定在轮 3 会变成一次
// 指向不存在路径的写入尝试，而错误信息只会说「路径不存在」。
func TestValidateClusterRejectsPartialGitBinding(t *testing.T) {
	c := validCluster()
	c.Git = &registry.GitBinding{RepoURL: "https://gitlab.example.com/net/policies.git"}
	if err := registry.ValidateCluster(c); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid for a git binding missing branch and path", err)
	}
}

func TestValidateClusterAcceptsCompleteGitBinding(t *testing.T) {
	c := validCluster()
	c.Git = &registry.GitBinding{
		RepoURL: "https://gitlab.example.com/net/policies.git",
		Branch:  "main", PolicyPath: "clusters/prod-asia-1",
	}
	if err := registry.ValidateCluster(c); err != nil {
		t.Errorf("ValidateCluster() error = %v, want nil", err)
	}
}
