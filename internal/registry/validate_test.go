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

// kubeconfigRef 复用 secrets.ValidateRef 而不是另写一份字符集校验，
// 这条用例锁住这个复用关系：路径穿越式的引用必须在入库前就被拦下。
//
// 它拦的是一个能指向凭据目录之外的引用。丢了这条校验不会有任何症状，
// 直到采集器拿着它读到别的东西 —— 而这条引用后面挂的是 kubeconfig，
// 读到别的东西意味着用一个不属于本集群的身份去对 apiserver 说话。
func TestValidateClusterRejectsMalformedKubeconfigRef(t *testing.T) {
	for name, ref := range map[string]string{
		"path traversal": "../escape",
		"slash":          "prod/asia",
		"uppercase":      "Prod-Asia-1",
		"too long":       strings.Repeat("a", 65),
	} {
		t.Run(name, func(t *testing.T) {
			c := validCluster()
			c.KubeconfigRef = ref
			err := registry.ValidateCluster(c)
			if !errors.Is(err, registry.ErrInvalid) {
				t.Fatalf("ValidateCluster(kubeconfigRef=%q) = %v, want ErrInvalid", ref, err)
			}
			if !strings.Contains(err.Error(), "kubeconfigRef") {
				t.Errorf("error %q does not name the offending field", err)
			}
		})
	}
}

// kubeconfigRef 可以为空：集群可以先登记下来，凭据稍后再配 ——
// 与 GitRepo.CredentialRef 同一条规则。
func TestValidateClusterAllowsEmptyKubeconfigRef(t *testing.T) {
	c := validCluster()
	c.KubeconfigRef = ""
	if err := registry.ValidateCluster(c); err != nil {
		t.Fatalf("ValidateCluster() = %v, want nil", err)
	}
}

func TestValidateClusterAcceptsAWellFormedKubeconfigRef(t *testing.T) {
	c := validCluster()
	c.KubeconfigRef = "prod-asia-1-kubeconfig"
	if err := registry.ValidateCluster(c); err != nil {
		t.Fatalf("ValidateCluster() = %v, want nil", err)
	}
}

// 绑定的合法性必须与集群其余字段无关：一个 podCidr 写错的集群，
// 其绑定仍然应当能被独立校验。这条正是拆分要买到的东西。
func TestValidateGitBindingIsIndependentOfTheCluster(t *testing.T) {
	b := registry.GitBinding{
		RepoID:     "policies-prod",
		PolicyPath: "clusters/prod-asia-1",
	}
	if err := registry.ValidateGitBinding(b); err != nil {
		t.Fatalf("ValidateGitBinding() = %v, want nil", err)
	}
}

// ValidateCluster 不得再看绑定：集群字段全对时，一个非法绑定
// 不应该让集群校验失败——它已经不是集群的一部分了。
func TestValidateClusterNoLongerJudgesTheBinding(t *testing.T) {
	c := validCluster()
	c.Git = &registry.GitBinding{RepoID: "policies-prod"} // 非法：缺 policyPath
	if err := registry.ValidateCluster(c); err != nil {
		t.Fatalf("ValidateCluster() = %v, want nil", err)
	}
}

// Git 绑定要么完整，要么不填。填一半的绑定在轮 3 会变成一次
// 指向不存在路径的写入尝试，而错误信息只会说「路径不存在」。
func TestValidateGitBindingRejectsPartialBinding(t *testing.T) {
	for name, b := range map[string]registry.GitBinding{
		"repo without path": {RepoID: "policies-prod"},
		"path without repo": {PolicyPath: "clusters/prod-asia-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := registry.ValidateGitBinding(b); !errors.Is(err, registry.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid for a partial git binding", err)
			}
		})
	}
}

// validGitBinding 是一个完整的、能通过校验的 Git 绑定，供只需要
// 改动绑定里单个字段的用例复用。
func validGitBinding() registry.GitBinding {
	return registry.GitBinding{
		RepoID:     "policies-prod",
		PolicyPath: "clusters/prod-asia-1",
	}
}

func TestValidateGitBindingAcceptsACompleteBinding(t *testing.T) {
	if err := registry.ValidateGitBinding(validGitBinding()); err != nil {
		t.Errorf("ValidateGitBinding() error = %v, want nil", err)
	}
}

// 路径级 verifyResult 是封闭枚举：一个未登记的取值不能悄悄存进去，
// 否则前端文案映射与统计口径都对不上号。
//
// 仓库级的取值在这一层同样是「未登记」：AUTH_FAILED 存在绑定上，读的人
// 会以为那是关于 policyPath 的判断（design doc 2026-08-13 §3.3）。
func TestValidateGitBindingRejectsVerdictsThatAreNotPathLevel(t *testing.T) {
	for _, vr := range []registry.BindingVerifyResult{
		"PROBABLY_FINE", "AUTH_FAILED", "CREDENTIAL_UNRESOLVED",
		"REPO_UNREACHABLE", "BRANCH_MISSING",
	} {
		b := validGitBinding()
		b.VerifyResult = vr
		if err := registry.ValidateGitBinding(b); !errors.Is(err, registry.ErrInvalid) {
			t.Errorf("ValidateGitBinding(verifyResult=%q) = %v, want ErrInvalid", vr, err)
		}
	}
}

// validGitRepo 是一个完整的、能通过校验的策略仓库，供只需要改动
// 单个字段的用例复用。
func validGitRepo() registry.GitRepo {
	return registry.GitRepo{ //nolint:gosec // G101 false positive: CredentialRef holds a Secret Manager name, not a credential.
		ID:            "policies-prod",
		URL:           "ssh://git@gitlab.example.com/net/policies.git",
		Branch:        "main",
		CredentialRef: "prod-asia-1-git",
	}
}

func TestValidateGitRepoAcceptsACompleteRepo(t *testing.T) {
	if err := registry.ValidateGitRepo(validGitRepo()); err != nil {
		t.Errorf("ValidateGitRepo() error = %v, want nil", err)
	}
}

// 仓库要么完整，要么不填：缺 branch 的仓库会让校验去问一个没有名字的
// 分支，而失败会被报成 BRANCH_MISSING —— 一句指着操作者没填的东西说
// 「它不存在」的话。
func TestValidateGitRepoRejectsPartialRepo(t *testing.T) {
	for name, mutate := range map[string]func(*registry.GitRepo){
		"missing id":     func(r *registry.GitRepo) { r.ID = "" },
		"missing url":    func(r *registry.GitRepo) { r.URL = "" },
		"missing branch": func(r *registry.GitRepo) { r.Branch = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r := validGitRepo()
			mutate(&r)
			if err := registry.ValidateGitRepo(r); !errors.Is(err, registry.ErrInvalid) {
				t.Errorf("ValidateGitRepo() = %v, want ErrInvalid", err)
			}
		})
	}
}

// 仓库级 verifyResult 是封闭枚举，且路径级的取值在这一层不合法：
// 仓库上写着 PATH_MISSING，说的是一件仓库级校验从未看过的事。
func TestValidateGitRepoRejectsVerdictsThatAreNotRepoLevel(t *testing.T) {
	for _, vr := range []registry.RepoVerifyResult{"PROBABLY_FINE", "PATH_MISSING"} {
		r := validGitRepo()
		r.VerifyResult = vr
		if err := registry.ValidateGitRepo(r); !errors.Is(err, registry.ErrInvalid) {
			t.Errorf("ValidateGitRepo(verifyResult=%q) = %v, want ErrInvalid", vr, err)
		}
	}
}

// credentialRef 复用 secrets.ValidateRef 而不是另写一份字符集校验，
// 这条用例锁住这个复用关系：路径穿越式的引用必须在这里就被拦下。
//
// 字段从绑定搬到了仓库，这条校验必须跟着搬 —— 它拦的是一个能指向凭据
// 目录之外的引用，丢了不会有任何症状，直到有人用它读到别的东西。
func TestValidateGitRepoRejectsMalformedCredentialRef(t *testing.T) {
	r := validGitRepo()
	r.CredentialRef = "../escape"
	err := registry.ValidateGitRepo(r)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("ValidateGitRepo() = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "credentialRef") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

// credentialRef 可以为空：仓库可以先登记下来，凭据稍后再配。
func TestValidateGitRepoAllowsEmptyCredentialRef(t *testing.T) {
	r := validGitRepo()
	r.CredentialRef = ""
	if err := registry.ValidateGitRepo(r); err != nil {
		t.Fatalf("ValidateGitRepo() = %v, want nil", err)
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

// 非 SSH 的 repoUrl 必须在保存时就被拒绝，而不是留到校验。
//
// 这条守的不是「样式统一」，是一句假结论：校验路径给 clone 挂的是 SSH
// 认证方法，而传输按 scheme 选 —— https:// 会在拨号之前就失败，几微秒
// 内返回，然后落进 REPO_UNREACHABLE。那是一句关于网络的结论，而网络
// 从未被碰过，操作者会被送去查一道不存在的防火墙（spec §2.2）。
//
// 报错文本要指名真正的原因：只说「地址不合法」等于把配置错误说成打字错。
//
// repoUrl 从绑定搬到了仓库（design doc 2026-08-13 §3.1），这条规则必须
// 跟着搬：搬家途中丢掉它，症状与从来没写过完全一样 —— 一个 https:// 的
// 仓库照常保存，然后在校验结果里显示「仓库不可达」。
func TestValidateGitRepoRejectsNonSSHURL(t *testing.T) {
	for _, url := range []string{
		"https://gitlab.example.com/net/policies.git",
		"http://gitlab.example.com/net/policies.git",
		"git://gitlab.example.com/net/policies.git",
		"file:///tmp/policies.git",
		"http://169.254.169.254/latest/meta-data/",
		"gitlab.example.com/net/policies.git",
		"ssh://",
		"",
	} {
		r := validGitRepo()
		r.URL = url
		err := registry.ValidateGitRepo(r)
		if !errors.Is(err, registry.ErrInvalid) {
			t.Errorf("ValidateGitRepo(repoUrl=%q) = %v, want ErrInvalid", url, err)
			continue
		}
		if url == "" {
			// 空值由「repoUrl 与 branch 必须同时填写」那条先接住，不必再谈 SSH。
			continue
		}
		var ie *registry.InvalidError
		if !errors.As(err, &ie) || !strings.Contains(ie.Detail, "SSH") {
			t.Errorf("ValidateGitRepo(repoUrl=%q) detail = %q, want it to name SSH as the real reason",
				url, ie.Detail)
		}
	}
}

// 平台真正会去连的两种写法都要收：scheme 式与 scp 式。
//
// 只认其中一种的话，另一种会被报成配置错误 —— 而它是对的地址，操作者
// 会去改一个没有错的东西。
func TestValidateGitRepoAcceptsTheSSHFormsThePlatformDials(t *testing.T) {
	for _, url := range []string{
		"ssh://git@gitlab.example.com/net/policies.git",
		"ssh://gitlab.example.com/net/policies.git",
		"ssh://git@gitlab.example.com:2222/net/policies.git",
		"SSH://git@gitlab.example.com/net/policies.git",
		"git@gitlab.example.com:net/policies.git",
	} {
		r := validGitRepo()
		r.URL = url
		if err := registry.ValidateGitRepo(r); err != nil {
			t.Errorf("ValidateGitRepo(repoUrl=%q) = %v, want nil", url, err)
		}
	}
}

// 双栈登记：逗号分隔的多段必须被接受。
//
// **校验与 cluster.ParsePrefixes 必须同一套判据**：一条这里放行、那边却解析
// 不出来的登记，会安静地落进「网段登记坏掉」而不是在提交时被拒 —— 而那时
// 症状是那个集群的 IP 归属全部退化成 UNKNOWN，成因在几天前的一次提交里。
func TestValidateClusterAcceptsDualStackCIDRs(t *testing.T) {
	c := validCluster()
	c.PodCIDR = "10.4.0.0/14, fd00:10:4::/56"
	c.NodeCIDR = "10.128.0.0/20,fd00:10:128::/64"
	if err := registry.ValidateCluster(c); err != nil {
		t.Errorf("ValidateCluster() = %v，双栈登记被拒了", err)
	}
}

// 一段写错就整条拒绝，且错误要点名是哪一段。
func TestValidateClusterRejectsABadSegment(t *testing.T) {
	c := validCluster()
	c.PodCIDR = "10.4.0.0/14, not-a-cidr"
	err := registry.ValidateCluster(c)
	if err == nil {
		t.Fatal("一段写错的网段被接受了")
	}
	if !strings.Contains(err.Error(), "not-a-cidr") {
		t.Errorf("错误没点名是哪一段：%v", err)
	}
}

// 多余逗号留下的空段同样拒绝：静默忽略会让人以为填对了，
// 而他少填的那一段正是双栈里的另一半。
func TestValidateClusterRejectsAnEmptySegment(t *testing.T) {
	c := validCluster()
	c.PodCIDR = "10.4.0.0/14,"
	if err := registry.ValidateCluster(c); err == nil {
		t.Error("多余逗号留下的空段被静默接受了")
	}
}
