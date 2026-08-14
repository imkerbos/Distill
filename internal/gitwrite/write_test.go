package gitwrite_test

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/imkerbos/Distill/internal/gitssh"
	"github.com/imkerbos/Distill/internal/gitwrite"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
)

// 测试仓库里的基底内容。策略目录下有两个文件，仓库根上还有一个与策略无关的
// 文件 —— 后两个都不在任何计划里，用来盯住「从不删除」。
const (
	policyPath   = "clusters/prod-asia-1"
	apiPath      = policyPath + "/api.yaml"
	keepPath     = policyPath + "/keep.yaml"
	readmePath   = "README.md"
	apiContent   = "kind: NetworkPolicy\nname: api\n"
	keepContent  = "kind: NetworkPolicy\nname: keep\n"
	readmeText   = "policies live here\n"
	deployBranch = "master"
	targetBranch = "distill/prod-asia-1-20260814T093000Z"
)

// stubResolver 代替 Secret Manager：返回一把当场生成的私钥，或一个错误，
// 并记下它被问过哪个引用。
//
// 记引用是为了断言出站确实去取过凭据 —— 一条根本没有认证的推送在 file://
// 上是跑得通的。
type stubResolver struct {
	key  []byte
	err  error
	asks []string
}

func (s *stubResolver) Resolve(_ context.Context, ref string) ([]byte, error) {
	s.asks = append(s.asks, ref)
	if s.err != nil {
		return nil, s.err
	}
	return s.key, nil
}

// 永不推送绑定里配置的那条分支：那条分支是 Config Sync 应用的对象。
func TestPushRefusesToTargetTheConfiguredBranch(t *testing.T) {
	cases := map[string]struct {
		deploy string
		target string
	}{
		"the deploy branch itself": {deploy: deployBranch, target: deployBranch},
		// 部署分支本身长得像一条 distill 分支时，前缀检查放行，只有这条
		// 判断挡得住 —— 这个子用例证明挡住它的确实是这条判断。
		"a deploy branch that looks like a distill branch": {
			deploy: "distill/live", target: "distill/live",
		},
		// 拿不到部署分支就无从判断目标是不是它，一律拒绝。
		"the deploy branch is unknown": {deploy: "", target: targetBranch},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			url, dir := newPolicyRepo(t)
			before := refSnapshot(t, dir)

			repo := testRepo(url, c.deploy)
			sha, err := newWriter(t, newResolver(t)).Push(
				context.Background(), repo, planWith(t, c.target, apiPath, "changed\n"))

			if !errors.Is(err, gitwrite.ErrTargetIsDeployBranch) {
				t.Fatalf("Push(target=%q, deploy=%q) error = %v, want ErrTargetIsDeployBranch",
					c.target, c.deploy, err)
			}
			if sha != "" {
				t.Errorf("Push() returned commit %q while refusing", sha)
			}
			if after := refSnapshot(t, dir); after != before {
				t.Errorf("remote refs changed on a refused push:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// 永不强制推送：目标分支已存在时失败，不覆盖、也不推着它往前走。
func TestPushNeverForces(t *testing.T) {
	cases := map[string]func(t *testing.T, url, dir string){
		// 已存在的分支指向一个与基底分叉的提交。一次带 force 的推送会把它
		// 覆盖掉，不带 force 的推送则被远端拒绝。
		"the branch exists and has diverged": func(t *testing.T, url, _ string) {
			seedDivergentBranch(t, url, targetBranch)
		},
		// 已存在的分支指向基底提交本身。这一种是**可以快进**的：不带 force
		// 的推送在协议上会被接受，只有推送之前那道存在性判断挡得住。
		"the branch exists and could be fast-forwarded": func(t *testing.T, _, dir string) {
			seedBranchAtHead(t, dir, targetBranch)
		},
	}

	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			url, dir := newPolicyRepo(t)
			seed(t, url, dir)
			before := refSnapshot(t, dir)

			sha, err := newWriter(t, newResolver(t)).Push(
				context.Background(), testRepo(url, deployBranch),
				planWith(t, targetBranch, apiPath, "changed by distill\n"))

			if err == nil {
				t.Fatalf("Push() onto an existing branch = %q, nil error, want refusal", sha)
			}
			if sha != "" {
				t.Errorf("Push() returned commit %q while refusing", sha)
			}
			if after := refSnapshot(t, dir); after != before {
				t.Errorf("an existing branch was moved by a push that must never force:\n"+
					"before:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// 内容与现有逐字节相同时不建分支、不提交、不推送。
// 空提交会把「平台真的改过什么」埋进噪声。
func TestIdenticalContentProducesNoCommit(t *testing.T) {
	url, dir := newPolicyRepo(t)
	before := refSnapshot(t, dir)

	plan := planWith(t, targetBranch, apiPath, apiContent)
	sha, err := newWriter(t, newResolver(t)).Push(context.Background(), testRepo(url, deployBranch), plan)

	if !errors.Is(err, gitwrite.ErrNothingToPush) {
		t.Fatalf("Push(identical content) error = %v, want ErrNothingToPush", err)
	}
	if sha != "" {
		t.Errorf("Push(identical content) returned commit %q, want no commit at all", sha)
	}
	after := refSnapshot(t, dir)
	if after != before {
		t.Errorf("identical content still touched the remote:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if strings.Contains(after, targetBranch) {
		t.Errorf("identical content created branch %q", targetBranch)
	}
}

// 只新增与更新，从不删除。仓库里多余的文件在推送后仍然在。
func TestPushNeverDeletesExistingFiles(t *testing.T) {
	url, dir := newPolicyRepo(t)

	// 计划只提到 api.yaml：keep.yaml 与 README.md 都不在计划里。
	plan := planWith(t, targetBranch, apiPath, "kind: NetworkPolicy\nname: api-v2\n")
	sha, err := newWriter(t, newResolver(t)).Push(context.Background(), testRepo(url, deployBranch), plan)
	if err != nil {
		t.Fatalf("Push() = %v, want nil", err)
	}

	pushed := treeOf(t, dir, targetBranch, sha)
	want := map[string]string{
		apiPath:    "kind: NetworkPolicy\nname: api-v2\n",
		keepPath:   keepContent,
		readmePath: readmeText,
	}
	for p, content := range want {
		got, ok := fileInTree(t, pushed, p)
		if !ok {
			t.Errorf("%q is gone from the pushed branch —— 删掉一个策略文件"+
				"等于把那一片从有规则变成默认拒绝", p)
			continue
		}
		if got != content {
			t.Errorf("%q content = %q, want %q", p, got, content)
		}
	}
}

// 推送落在一条新分支上，绑定里配置的那条分支纹丝不动。
func TestPushCreatesTheTargetBranchAndLeavesTheDeployBranchAlone(t *testing.T) {
	url, dir := newPolicyRepo(t)
	deployBefore := refHash(t, dir, plumbing.NewBranchReferenceName(deployBranch))

	// 新增一个此前不存在的文件，顺带覆盖「目录要现建」。
	newFile := policyPath + "/new/db.yaml"
	plan := planWith(t, targetBranch, newFile, "kind: NetworkPolicy\nname: db\n")
	sha, err := newWriter(t, newResolver(t)).Push(context.Background(), testRepo(url, deployBranch), plan)
	if err != nil {
		t.Fatalf("Push() = %v, want nil", err)
	}
	if err := registry.ValidateCommitSHA(sha); err != nil {
		t.Fatalf("Push() returned %q, which is not a full commit SHA: %v", sha, err)
	}

	head := refHash(t, dir, plumbing.NewBranchReferenceName(targetBranch))
	if head != sha {
		t.Errorf("pushed branch head = %q, want the returned commit %q", head, sha)
	}
	if got := refHash(t, dir, plumbing.NewBranchReferenceName(deployBranch)); got != deployBefore {
		t.Errorf("the deploy branch moved: %q -> %q", deployBefore, got)
	}

	commit := commitOf(t, dir, sha)
	// 提交作者用平台身份，不冒充操作者（design doc §7）。
	if commit.Author.Name != "distill" {
		t.Errorf("commit author = %q, want the platform identity", commit.Author.Name)
	}
	// 落进仓库历史的必须是计划里那段文字，逐字节相同。
	//
	// 提交信息进了指纹，因此它就是操作者确认过的那一份；写入端另拼一句，
	// 合并请求上的评审人读到的理由就不是被批准的那个 —— 而那句话会永久
	// 留在历史上。
	if commit.Message != plan.CommitMessage {
		t.Errorf("commit message = %q, want the plan's message %q —— "+
			"推出去的必须是操作者确认过的那一句", commit.Message, plan.CommitMessage)
	}
	if content, ok := fileInTree(t, treeOf(t, dir, targetBranch, sha), newFile); !ok || content == "" {
		t.Errorf("%q was not written to the pushed branch", newFile)
	}
}

// 出站必须经由 gitssh.Transport 取凭据。
//
// ErrUnusableCredential 只有 Transport.Auth 会产生：私钥解析失败在那里被换成
// 这个哨兵，底层错误不往上带。把认证换成裸的 gogitssh.NewPublicKeys，冒上来的
// 就是 go-git 自己的解析错误，这个断言随即变红 —— 读路径上出过的正是「守卫被
// 测到、但没有任何东西证明调用方还在调用它」，写路径行使的是写权限。
func TestPushAuthenticatesThroughTheGuardedTransport(t *testing.T) {
	url, dir := newPolicyRepo(t)
	before := refSnapshot(t, dir)

	r := &stubResolver{key: []byte("this is not a private key")}
	repo := testRepo(url, deployBranch)
	sha, err := newWriter(t, r).Push(context.Background(), repo,
		planWith(t, targetBranch, apiPath, "changed\n"))

	if !errors.Is(err, gitssh.ErrUnusableCredential) {
		t.Fatalf("Push(unusable credential) error = %v, want gitssh.ErrUnusableCredential —— "+
			"认证不经过 gitssh.Transport 时这里冒上来的会是别的错误", err)
	}
	if sha != "" {
		t.Errorf("Push() returned commit %q on an authentication failure", sha)
	}
	// 凭据在任何出站之前就要取：认证失败之后不得继续执行敏感操作。
	if len(r.asks) == 0 || r.asks[0] != repo.CredentialRef {
		t.Errorf("resolver was asked for %v, want the repo credential ref %q first", r.asks, repo.CredentialRef)
	}
	if after := refSnapshot(t, dir); after != before {
		t.Errorf("remote changed despite an authentication failure:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// 解析器自己的错误原样上抛：调用方要分辨「引用取不到」与「取到了但不可用」。
func TestPushPropagatesACredentialLookupFailure(t *testing.T) {
	url, _ := newPolicyRepo(t)
	want := errors.New("secret manager is down")

	_, err := newWriter(t, &stubResolver{err: want}).Push(
		context.Background(), testRepo(url, deployBranch),
		planWith(t, targetBranch, apiPath, "changed\n"))

	if !errors.Is(err, want) {
		t.Fatalf("Push() error = %v, want the resolver error", err)
	}
}

// 一份不写任何文件的计划推出去只会是一个空提交。
func TestPushRefusesAPlanWithNoFiles(t *testing.T) {
	url, dir := newPolicyRepo(t)
	before := refSnapshot(t, dir)

	plan := planWith(t, targetBranch, apiPath, "x\n")
	plan.Files = nil

	if _, err := newWriter(t, newResolver(t)).Push(
		context.Background(), testRepo(url, deployBranch), plan); !errors.Is(err, gitwrite.ErrNothingToPush) {
		t.Fatalf("Push(no files) error = %v, want ErrNothingToPush", err)
	}
	if after := refSnapshot(t, dir); after != before {
		t.Errorf("an empty plan touched the remote:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// 计划没带提交信息时拒绝，不就地补一句。
//
// 平台现编的那句话没有人批准过，却会永久留在仓库历史上，并且正是合并请求上
// 的评审人读来判断合不合的那句。
func TestPushRefusesAPlanWithoutACommitMessage(t *testing.T) {
	url, dir := newPolicyRepo(t)
	before := refSnapshot(t, dir)

	for _, msg := range []string{"", "   \n\t "} {
		plan := planWith(t, targetBranch, apiPath, "changed\n")
		plan.CommitMessage = msg

		if _, err := newWriter(t, newResolver(t)).Push(
			context.Background(), testRepo(url, deployBranch), plan); !errors.Is(err, gitwrite.ErrNoCommitMessage) {
			t.Fatalf("Push(commit message=%q) error = %v, want ErrNoCommitMessage", msg, err)
		}
	}
	if after := refSnapshot(t, dir); after != before {
		t.Errorf("a plan without a commit message touched the remote:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// 目标分支名必须是一条可用的 distill/* 分支名。
//
// 前缀这一条挡的是「计划里写了别的仓库的部署分支」：与本仓库配置的那条
// 同名才会被 ErrTargetIsDeployBranch 挡下，main 之类不会。
func TestPushRefusesATargetBranchOutsideTheDistillNamespace(t *testing.T) {
	url, dir := newPolicyRepo(t)
	before := refSnapshot(t, dir)

	for _, branch := range []string{
		"main",
		"release",
		"distill/",
		"distill/../main",
		"distill/prod\x00",
		"distill/prod asia",
		"refs/heads/distill/prod",
	} {
		t.Run(branch, func(t *testing.T) {
			_, err := newWriter(t, newResolver(t)).Push(
				context.Background(), testRepo(url, deployBranch),
				withBranch(planWith(t, targetBranch, apiPath, "changed\n"), branch))
			if !errors.Is(err, gitwrite.ErrInvalidTargetBranch) {
				t.Fatalf("Push(branch=%q) error = %v, want ErrInvalidTargetBranch", branch, err)
			}
		})
	}
	if after := refSnapshot(t, dir); after != before {
		t.Errorf("a refused branch name touched the remote:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// 计划批准的是哪个仓库，就只能写到哪个仓库（design doc §4，2026-08-15 修订）。
//
// 仓库标识进指纹之后，上层的指纹比对已经挡得住"绑定在确认与推送之间改指到
// 另一个仓库"；这一道在发起写的那一步再确认一次，理由与"永不推部署分支"
// 同源 —— 守卫必须紧贴真正把内容送出去的那一行。
func TestPushRefusesAPlanApprovedForAnotherRepository(t *testing.T) {
	url, dir := newPolicyRepo(t)
	before := refSnapshot(t, dir)

	cases := map[string]func(t *testing.T) registry.WritebackPlan{
		"另一个仓库": func(t *testing.T) registry.WritebackPlan {
			t.Helper()
			return planFor(t,
				registry.GitBinding{RepoID: "repo-somewhere-else", PolicyPath: policyPath},
				targetBranch, apiPath, "changed\n")
		},
		// 没说清落点的计划，它的指纹里也就没有落点这一维。构造函数造不出这种
		// 计划（绑定必须带 repoId），因此就地抹掉 —— 本包拿不到绑定，复核不了
		// 上游，一份缺了落点的计划走到这里时唯一安全的反应是拒绝。
		"计划没说落点": func(t *testing.T) registry.WritebackPlan {
			t.Helper()
			p := planWith(t, targetBranch, apiPath, "changed\n")
			p.RepoID = ""
			return p
		},
	}
	for name, makePlan := range cases {
		t.Run(name, func(t *testing.T) {
			sha, err := newWriter(t, newResolver(t)).Push(
				context.Background(), testRepo(url, deployBranch), makePlan(t))

			if !errors.Is(err, gitwrite.ErrPlanRepoMismatch) {
				t.Fatalf("Push(%s) error = %v, want ErrPlanRepoMismatch", name, err)
			}
			if sha != "" {
				t.Errorf("Push() returned commit %q while refusing", sha)
			}
		})
	}
	if after := refSnapshot(t, dir); after != before {
		t.Errorf("a plan approved for another repository touched the remote:\nbefore:\n%s\nafter:\n%s",
			before, after)
	}
}

// 「永不强制」这条必须绑住**调用点**，而不只是绑住 pushOptions 的取值。
//
// 本包里 checkTargetAbsent 先把已存在的分支挡下了，于是推送那一层平时见不到
// 冲突 —— 把 local.PushContext 的参数换成一个内联的 Force: true，其余用例
// 全绿。这正是本项目反复出现的那类缺陷：守卫被单独测到，没有东西证明调用点
// 还在经过它。
//
// 这里把存在性判断那一层摘掉：远端配 uploadpack.hideRefs，目标分支于是不出现
// 在 ls-remote 的通告里，checkTargetAbsent 看不见它并放行；而 receive-pack 仍然
// 看得见（它读的是 receive.hideRefs），推送那一层因此**单独承压**。这与
// checkTargetAbsent 注释里承认的那个时间窗是同一件事：判断与推送之间新建的
// 同名分支，只有推送那一层挡得住。
//
// 远端同时把 receive.denyNonFastForwards 显式关掉：拦住这次覆盖的必须是平台
// 自己不发 force，不是远端恰好拒绝。
func TestPushNeverForcesWhenTheExistenceCheckCannotSeeTheBranch(t *testing.T) {
	url, dir := newPolicyRepo(t)
	seedDivergentBranch(t, url, targetBranch)
	hideBranchesFromLsRemote(t, dir)

	before := refSnapshot(t, dir)
	existing := refHash(t, dir, plumbing.NewBranchReferenceName(targetBranch))

	sha, err := newWriter(t, newResolver(t)).Push(
		context.Background(), testRepo(url, deployBranch),
		planWith(t, targetBranch, apiPath, "changed by distill\n"))

	if err == nil {
		t.Fatalf("Push() onto a hidden existing branch = %q, nil error, want refusal", sha)
	}
	if got := refHash(t, dir, plumbing.NewBranchReferenceName(targetBranch)); got != existing {
		t.Errorf("the existing branch was overwritten: %q -> %q —— "+
			"推送那一层没有拦住它，说明调用点没有在用 pushOptions 的 Force: false",
			existing, got)
	}
	if after := refSnapshot(t, dir); after != before {
		t.Errorf("a refused push changed the remote:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// 目的地址判定必须管住**真实的那次拨号**，不能只管 URL 里的字符串。
//
// 读路径上早有这条（gitverify 的 TestVerifyRefusesToConnectToAnInternalAddress
// 与 TestCloneBranchDialsThroughTheGuardedAuth），写路径此前没有复用这套手法，
// 于是 ledger 把「真实拨号时的地址守卫」整格记成未验证 —— 对读路径而言那句话
// 是不准的，对写路径而言只是没做。这条把写路径这一格补上。
//
// 在回环地址上起一个真的 SSH 服务端，并把它的 host key 钉进清单（即 host key
// 那一层是放行的），此刻唯一还能拦住这次连接的就是目的地址判定；断言拿到的
// 是 gitssh.ErrBlockedDestination 这个哨兵，而不是「握手失败」——任何一次失败
// 的握手都会返回某个错误，按「有没有出错」判等于什么都没判。
//
// **它证明不到的那一格**（与读路径同）：钉死的 host key 清单在这里从未被比对，
// 地址判定先跑，握手在比对之前就断了。deploy key 的写权限同样一次都没被行使。
func TestPushRefusesToConnectToAnInternalAddress(t *testing.T) {
	authAttempted := make(chan struct{}, 1)
	addr, hostKey := startSSHListener(t, authAttempted)

	known := []byte(knownhosts.Normalize(addr) + " " + string(cryptossh.MarshalAuthorizedKey(hostKey)))
	w, err := gitwrite.New(newResolver(t), known, 10*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	repo := registry.GitRepo{
		ID: "repo-1", URL: "ssh://git@" + addr + "/policies.git",
		Branch: deployBranch, CredentialRef: keyRef,
	}
	sha, err := w.Push(context.Background(), repo,
		planWith(t, targetBranch, apiPath, "changed\n"))

	if !errors.Is(err, gitssh.ErrBlockedDestination) {
		t.Fatalf("Push(loopback) error = %v, want gitssh.ErrBlockedDestination —— "+
			"这次拨号没有经过 Transport.Auth 挂上去的目的地址判定", err)
	}
	if sha != "" {
		t.Errorf("Push() returned commit %q while the dial was refused", sha)
	}
	// 判定失效时客户端会一路走到认证，服务端的认证回调随即被触发；判定生效
	// 时握手在密钥交换阶段就断了，服务端永远等不到那一步。
	select {
	case <-authAttempted:
		t.Fatal("the platform authenticated against a loopback address: " +
			"the destination guard did not govern the real connection")
	case <-time.After(500 * time.Millisecond):
	}
}

// 枚举只读：报出策略路径下已有的文件与现存的 distill/* 分支，且一个引用都不动
// （design doc §2、§3）。
//
// 计划里那两份清单就由它填，而「（无）多余文件」这句话在界面上是一条事实陈述 ——
// 它必须来自平台真的看过一次仓库。
func TestListReportsTheRepositoryWithoutTouchingIt(t *testing.T) {
	url, dir := newPolicyRepo(t)
	before := refSnapshot(t, dir)

	got, err := newWriter(t, newResolver(t)).List(
		context.Background(), testRepo(url, deployBranch), policyPath)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}

	// 策略路径下的两个文件都在；仓库根上那个 README.md 不在 —— 边界落在
	// policyPath 上，不是整个仓库。
	if want := []string{apiPath, keepPath}; !slices.Equal(got.Files, want) {
		t.Errorf("List() files = %v, want %v", got.Files, want)
	}
	// newPolicyRepo 里那条 distill/live 是现存的 distill 分支；部署分支不是。
	if want := []string{"distill/live"}; !slices.Equal(got.Branches, want) {
		t.Errorf("List() branches = %v, want %v", got.Branches, want)
	}
	if after := refSnapshot(t, dir); after != before {
		t.Errorf("listing changed the remote refs:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// policyPath 下同名前缀的兄弟目录不算在内：判定按路径段边界走，不是前缀比对。
//
// "clusters/prod-asia-1-old/api.yaml" 若被列成多余文件，操作者会去删一个属于
// 另一个集群的目录。
func TestListDoesNotReachOutsideThePolicyPath(t *testing.T) {
	url, _ := newPolicyRepo(t)
	seedFileOnDeployBranch(t, url, policyPath+"-old/api.yaml", "kind: NetworkPolicy\n")

	got, err := newWriter(t, newResolver(t)).List(
		context.Background(), testRepo(url, deployBranch), policyPath)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	for _, f := range got.Files {
		if !strings.HasPrefix(f, policyPath+"/") {
			t.Errorf("List() reported %q, which is outside the policy path %q", f, policyPath)
		}
	}
}

// 枚举也必须经由 gitssh.Transport 取凭据：它同样是一次出站。
//
// 与推送那条同一个判据 —— ErrUnusableCredential 只有 Transport.Auth 会产生。
func TestListAuthenticatesThroughTheGuardedTransport(t *testing.T) {
	url, _ := newPolicyRepo(t)

	r := &stubResolver{key: []byte("this is not a private key")}
	repo := testRepo(url, deployBranch)
	if _, err := newWriter(t, r).List(context.Background(), repo, policyPath); !errors.Is(
		err, gitssh.ErrUnusableCredential) {
		t.Fatalf("List(unusable credential) error = %v, want gitssh.ErrUnusableCredential", err)
	}
	if len(r.asks) == 0 || r.asks[0] != repo.CredentialRef {
		t.Errorf("resolver was asked for %v, want the repo credential ref %q first", r.asks, repo.CredentialRef)
	}
}

// 策略路径在仓库里还不存在时报空清单，不报错：那只意味着这次写回会新建这个
// 目录，没有多余文件可报。
func TestListReportsNoFilesForAPolicyPathThatDoesNotExist(t *testing.T) {
	url, _ := newPolicyRepo(t)

	got, err := newWriter(t, newResolver(t)).List(
		context.Background(), testRepo(url, deployBranch), "clusters/not-there")
	if err != nil {
		t.Fatalf("List(missing policy path) = %v, want nil", err)
	}
	if len(got.Files) != 0 {
		t.Errorf("List() files = %v, want none", got.Files)
	}
}

// --- helpers ---

// withBranch 换掉计划的目标分支。
//
// 不重算指纹：指纹是操作者二次确认用的，由 httpapi 校验，不是本包判断的
// 东西 —— 在这里重算会让这些用例看起来像在测一件本包不负责的事。
func withBranch(p registry.WritebackPlan, branch string) registry.WritebackPlan {
	p.Branch = branch
	return p
}

// planWith 造一份合法的写回计划：单个文件，四类计数齐全。
//
// 走 registry.NewWritebackPlan 而不是直接填结构体：那里做的是路径包含判断，
// 本包不复核它，测试就不该绕过它 —— 否则这些用例会在一份生产上根本构造不
// 出来的计划上通过。
func planWith(t *testing.T, branch, filePath, content string) registry.WritebackPlan {
	t.Helper()
	return planFor(t,
		registry.GitBinding{RepoID: testRepoID, PolicyPath: policyPath},
		branch, filePath, content)
}

// planFor 是换一个绑定的版本：计划上的仓库标识由绑定决定（进指纹的那一维），
// 因此"一份批准给另一个仓库的计划"只有换绑定才造得出来。
func planFor(
	t *testing.T, binding registry.GitBinding, branch, filePath, content string,
) registry.WritebackPlan {
	t.Helper()
	counts := make(map[predict.ChangeKind]int, 4)
	for i, k := range predict.AllChangeKinds() {
		counts[k] = i
	}
	p, err := registry.NewWritebackPlan(binding, registry.WritebackPlan{
		Branch:        branch,
		Files:         []registry.WritebackFile{{Path: filePath, Content: content}},
		Counts:        counts,
		CommitMessage: testCommitMessage,
	})
	if err != nil {
		t.Fatalf("NewWritebackPlan() = %v", err)
	}
	return p
}

// keyRef 是测试用的凭据引用。它指向 stubResolver，不指向任何真实的 Secret。
const keyRef = "ref-for-tests"

// testRepoID 是测试仓库的标识。计划上的 RepoID 必须与它相等，否则写入端拒绝 ——
// 计划批准的是哪个仓库，就只能写到哪个仓库。
const testRepoID = "repo-1"

// testCommitMessage 是计划携带的提交信息，形状与 Task 4 会生成的一致：
// 集群、时间窗、四类计数、发起者、平台版本，不含凭据与任何内部地址。
const testCommitMessage = "policy writeback for prod-asia-1\n\n" +
	"window: 2026-08-07..2026-08-14\nfiles: 1\n" +
	"WOULD_BREAK: 0\nWOULD_OPEN: 2\nUNCHANGED: 41\nUNKNOWN: 3\n" +
	"requested by: alice\nplatform: dev\n"

func testRepo(url, branch string) registry.GitRepo {
	return registry.GitRepo{ID: testRepoID, URL: url, Branch: branch, CredentialRef: keyRef}
}

func newWriter(t *testing.T, r *stubResolver) *gitwrite.Writer {
	t.Helper()
	w, err := gitwrite.New(r, hostKeyLine(t), 30*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return w
}

func newResolver(t *testing.T) *stubResolver {
	t.Helper()
	return &stubResolver{key: privateKeyPEM(t)}
}

// privateKeyPEM 当场生成一把 ed25519 私钥，只在内存里。
func privateKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := cryptossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(block)
}

// hostKeyLine 生成一行 known_hosts 格式的已知主机公钥。
func hostKeyLine(t *testing.T) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	key, err := cryptossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap host key: %v", err)
	}
	return append([]byte("git.example.com "), cryptossh.MarshalAuthorizedKey(key)...)
}

// newPolicyRepo 建一个带内容的裸仓库，返回它的 file:// 地址与目录。
//
// 先在工作区仓库里造出提交，再裸克隆一份当远端 —— 远端是裸的，形态与真实
// 的策略仓库一致，也才推得进去。
func newPolicyRepo(t *testing.T) (url, dir string) {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	repo, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	writeSeedFile(t, seed, apiPath, apiContent)
	writeSeedFile(t, seed, keepPath, keepContent)
	writeSeedFile(t, seed, readmePath, readmeText)

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := w.Commit("seed", &git.CommitOptions{Author: testSignature()}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	bare := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: "file://" + seed}); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	// 一条与部署分支同内容、名字长得像 distill 分支的分支，供「部署分支
	// 本身就叫 distill/*」那个子用例使用。
	seedBranchAtHead(t, bare, "distill/live")
	return "file://" + bare, bare
}

func writeSeedFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func testSignature() *object.Signature {
	return &object.Signature{Name: "seed", Email: "seed@example.com", When: time.Unix(0, 0).UTC()}
}

// seedBranchAtHead 在裸仓库里把一条分支指到部署分支当前的提交上。
//
// 这样造出来的分支对本次推送是**可以快进**的：不带 force 的推送在协议上会
// 被接受，因此它测的是推送之前那道存在性判断。
func seedBranchAtHead(t *testing.T, bare, branch string) {
	t.Helper()
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	head, err := repo.Reference(plumbing.NewBranchReferenceName(deployBranch), true)
	if err != nil {
		t.Fatalf("resolve %s: %v", deployBranch, err)
	}
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), head.Hash())
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("set %s: %v", branch, err)
	}
}

// seedDivergentBranch 在远端造一条与部署分支分叉的同名分支。
//
// 它模拟的是「别人已经用了这个分支名」：一次带 force 的推送会把这个提交
// 覆盖掉，而不带 force 的推送会被远端拒绝。
func seedDivergentBranch(t *testing.T, url, branch string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{URL: url})
	if err != nil {
		t.Fatalf("clone for divergence: %v", err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch), Create: true,
	}); err != nil {
		t.Fatalf("checkout %s: %v", branch, err)
	}
	writeSeedFile(t, dir, "someone-elses-work.txt", "not written by distill\n")
	if _, err := w.Add("someone-elses-work.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := w.Commit("someone else", &git.CommitOptions{Author: testSignature()}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	spec := config.RefSpec(plumbing.NewBranchReferenceName(branch).String() + ":" +
		plumbing.NewBranchReferenceName(branch).String())
	if err := repo.Push(&git.PushOptions{RefSpecs: []config.RefSpec{spec}}); err != nil {
		t.Fatalf("push divergence: %v", err)
	}
}

// hideBranchesFromLsRemote 让远端的 distill/* 分支不出现在 ls-remote 的通告里，
// 同时显式允许非快进的引用更新。
//
// `uploadpack.hideRefs` 只作用于 upload-pack（ls-remote 与 clone 走的那条），
// receive-pack 读的是 `receive.hideRefs`，因此推送那一层照样看得见这条分支 ——
// 这正是「存在性判断之后分支才出现」那个时间窗的等价形态。
//
// `receive.denyNonFastForwards` 显式关掉：拦住覆盖的必须是平台自己不发 force，
// 不是远端恰好拒绝。远端越宽松，这条用例证明的东西越强。
//
// 依赖 git 二进制（go-git 的 file:// 传输执行 git-upload-pack / git-receive-pack），
// 本包既有的全部用例已经依赖它，这里没有引入新的前提。
func hideBranchesFromLsRemote(t *testing.T, bare string) {
	t.Helper()
	repo, err := git.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Raw.Section("uploadpack").SetOption("hideRefs", "refs/heads/"+"distill/")
	cfg.Raw.Section("receive").SetOption("denyNonFastForwards", "false")
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatalf("set config: %v", err)
	}
}

// seedFileOnDeployBranch 往远端的部署分支上再加一个文件。
func seedFileOnDeployBranch(t *testing.T, url, rel, content string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{URL: url})
	if err != nil {
		t.Fatalf("clone for seeding: %v", err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	writeSeedFile(t, dir, rel, content)
	if _, err := w.Add(rel); err != nil {
		t.Fatalf("add %s: %v", rel, err)
	}
	if _, err := w.Commit("seed "+rel, &git.CommitOptions{Author: testSignature()}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ref := plumbing.NewBranchReferenceName(deployBranch)
	if err := repo.Push(&git.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(ref.String() + ":" + ref.String())},
	}); err != nil {
		t.Fatalf("push seed: %v", err)
	}
}

// startSSHListener 在回环地址上起一个只做握手的 SSH 服务端，返回它的地址与
// host key。任何一次公钥认证尝试都会往 attempted 里投一个信号。
//
// 与 gitverify 的那一份同形：客户端要走到 HostKeyCallback，服务端就必须真的把
// host key 交出来 —— 一个只 Accept 就关掉的监听给不出这一步，错误会停在
// 「握手失败：EOF」，那证明不了目的地址判定跑过。
//
// 没有把它抽成共享的测试工具包：两处各自起的是自己那条出站链路的服务端，
// 而"写路径有没有真的经过守卫"这件事，正是不能靠共享代码来回答的。
func startSSHListener(t *testing.T, attempted chan<- struct{}) (string, cryptossh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("wrap host key: %v", err)
	}

	cfg := &cryptossh.ServerConfig{
		PublicKeyCallback: func(cryptossh.ConnMetadata, cryptossh.PublicKey) (*cryptossh.Permissions, error) {
			select {
			case attempted <- struct{}{}:
			default:
			}
			return nil, errors.New("no")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				sc, chans, reqs, err := cryptossh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer func() { _ = sc.Close() }()
				go cryptossh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(cryptossh.Prohibited, "")
				}
			}()
		}
	}()

	return ln.Addr().String(), signer.PublicKey()
}

// refSnapshot 把仓库当前的全部引用与其指向渲染成一个字符串，用于比对推送
// 前后远端有没有被改动。
func refSnapshot(t *testing.T, dir string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	iter, err := repo.References()
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	var lines []string
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		lines = append(lines, ref.String())
		return nil
	}); err != nil {
		t.Fatalf("iterate references: %v", err)
	}
	// 引用的遍历顺序不保证稳定（松散与打包的引用来源不同），排序后再比。
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func refHash(t *testing.T, dir string, name plumbing.ReferenceName) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	ref, err := repo.Reference(name, true)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return ref.Hash().String()
}

func commitOf(t *testing.T, dir, sha string) *object.Commit {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		t.Fatalf("commit %s: %v", sha, err)
	}
	return commit
}

// treeOf 取出推送上去的那个提交的顶层 tree，并确认分支确实指向它。
func treeOf(t *testing.T, dir, branch, sha string) *object.Tree {
	t.Helper()
	if got := refHash(t, dir, plumbing.NewBranchReferenceName(branch)); got != sha {
		t.Fatalf("branch %s = %q, want the pushed commit %q", branch, got, sha)
	}
	tree, err := commitOf(t, dir, sha).Tree()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	return tree
}

func fileInTree(t *testing.T, tree *object.Tree, p string) (string, bool) {
	t.Helper()
	f, err := tree.File(p)
	if err != nil {
		return "", false
	}
	content, err := f.Contents()
	if err != nil {
		t.Fatalf("contents of %s: %v", p, err)
	}
	return content, true
}
