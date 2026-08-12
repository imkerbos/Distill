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
			// 只认这一条：错误必须点名出问题的那类网段。此前还有一条
			// 「或者文本里含 cidr」的兜底，而四条用例的文案全都含 cidr ——
			// 那等于这条断言永远为真，四类网段互换文案也测不出来。
			if err != nil && !strings.Contains(err.Error(), name) {
				t.Errorf("err = %q, want it to name the offending field %q", err, name)
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

// validClusterWithGit 是一个完整的、已通过校验的 Git 绑定，供只需要
// 改动绑定里单个字段的用例复用。
func validClusterWithGit() registry.Cluster {
	c := validCluster()
	c.Git = &registry.GitBinding{ //nolint:gosec // G101 false positive: CredentialRef holds a Secret Manager name, not a credential.
		RepoURL:       "https://gitlab.example.com/net/policies.git",
		Branch:        "main",
		PolicyPath:    "clusters/prod-asia-1",
		CredentialRef: "prod-asia-1-git",
	}
	return c
}

// verifyResult 是封闭枚举：一个未登记的取值不能悄悄存进去，否则前端
// 文案映射与统计口径都对不上号。
func TestValidateClusterRejectsUnknownVerifyResult(t *testing.T) {
	c := validClusterWithGit()
	c.Git.VerifyResult = "PROBABLY_FINE"
	if err := registry.ValidateCluster(c); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("ValidateCluster() = %v, want ErrInvalid", err)
	}
}

// credentialRef 复用 secrets.ValidateRef 而不是另写一份字符集校验，
// 这条用例锁住这个复用关系：路径穿越式的引用必须在这里就被拦下。
func TestValidateClusterRejectsMalformedCredentialRef(t *testing.T) {
	c := validClusterWithGit()
	c.Git.CredentialRef = "../escape"
	err := registry.ValidateCluster(c)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("ValidateCluster() = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "credentialRef") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

// credentialRef 可以为空：绑定可以先记下来，凭据稍后再配。
func TestValidateClusterAllowsEmptyCredentialRef(t *testing.T) {
	c := validClusterWithGit()
	c.Git.CredentialRef = ""
	if err := registry.ValidateCluster(c); err != nil {
		t.Fatalf("ValidateCluster() = %v, want nil", err)
	}
}

func validImport() registry.PolicyImport {
	return registry.PolicyImport{
		ClusterID: "prod-asia-1", ImportID: "imp-1", Plane: "networkpolicy",
		Role: registry.RoleBaselineCurrent, Source: registry.SourcePaste,
		Namespace: "payment", Name: "allow-gateway", SpecHash: "abc",
	}
}

// 来源与 commit 必须互相印证（spec §4）。
//
// 少了这条校验，{"source":"GIT","gitCommitSha":""} 会以 GIT / NULL 落库：
// 界面把它当作「现状」，而轮 3 的漂移检测要拿 commit 去仓库里定位一次
// 提交 —— 一条没有 commit 的 GIT 记录是一句无法核验的溯源声明。
func TestValidatePolicyImportChecksSourceAgainstCommit(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	cases := map[string]struct {
		mutate  func(*registry.PolicyImport)
		wantErr bool
	}{
		"paste without commit":  {func(*registry.PolicyImport) {}, false},
		"git with full commit":  {func(p *registry.PolicyImport) { p.Source = registry.SourceGit; p.GitCommitSHA = sha }, false},
		"git without commit":    {func(p *registry.PolicyImport) { p.Source = registry.SourceGit }, true},
		"git with short commit": {func(p *registry.PolicyImport) { p.Source = registry.SourceGit; p.GitCommitSHA = sha[:7] }, true},
		"git with non-hex commit": {func(p *registry.PolicyImport) {
			p.Source = registry.SourceGit
			p.GitCommitSHA = strings.Repeat("z", 40)
		}, true},
		"paste carrying a commit":   {func(p *registry.PolicyImport) { p.GitCommitSHA = sha }, true},
		"cluster carrying a commit": {func(p *registry.PolicyImport) { p.Source = registry.SourceCluster; p.GitCommitSHA = sha }, true},
		"unregistered role":         {func(p *registry.PolicyImport) { p.Role = "CURRENT" }, true},
		"unregistered source":       {func(p *registry.PolicyImport) { p.Source = "UPLOAD" }, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := validImport()
			tc.mutate(&p)
			err := registry.ValidatePolicyImport(p)
			if tc.wantErr && !errors.Is(err, registry.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

// 校验通过的 GIT 记录必须被 VerifiedAgainstGit 认出来：校验规则与界面
// 上那句「已用 Git 核对」说的必须是同一件事，否则一条通过了校验的记录
// 仍然可能在界面上显示成未核对。
func TestValidGitImportReportsItselfVerified(t *testing.T) {
	p := validImport()
	p.Source = registry.SourceGit
	p.GitCommitSHA = "0123456789abcdef0123456789abcdef01234567"
	if err := registry.ValidatePolicyImport(p); err != nil {
		t.Fatalf("ValidatePolicyImport() error = %v", err)
	}
	if !p.VerifiedAgainstGit() {
		t.Error("a validated GIT import does not report itself as verified against Git")
	}
}
