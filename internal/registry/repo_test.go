package registry_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

// RepoVerifyResult 的字面值必须钉死：它们既是 git_repo.verify_result 列的
// 取值，也是前端文案映射的键。改一个字母不会有编译错误，症状是界面上
// 校验结论显示空白，或统计口径悄悄漏计一类结果。
func TestRepoVerifyResultLiteralsArePinned(t *testing.T) {
	for got, want := range map[registry.RepoVerifyResult]string{
		registry.RepoVerifyNotVerified:          "NOT_VERIFIED",
		registry.RepoVerifyOK:                   "OK",
		registry.RepoVerifyCredentialUnresolved: "CREDENTIAL_UNRESOLVED",
		registry.RepoVerifyAuthFailed:           "AUTH_FAILED",
		registry.RepoVerifyRepoUnreachable:      "REPO_UNREACHABLE",
		registry.RepoVerifyBranchMissing:        "BRANCH_MISSING",
	} {
		if string(got) != want {
			t.Errorf("RepoVerifyResult literal = %q, want %q", got, want)
		}
	}
}

// 六个登记过的取值必须全部 Valid()，一个随手编的取值必须不是。
// 这条锁住 Valid() 的显式 switch 与常量列表保持同步 —— 加一个常量却
// 忘了把它加进 switch，这条测试会先红。
func TestRepoVerifyResultEnumIsClosed(t *testing.T) {
	known := []registry.RepoVerifyResult{
		registry.RepoVerifyNotVerified, registry.RepoVerifyOK,
		registry.RepoVerifyCredentialUnresolved, registry.RepoVerifyAuthFailed,
		registry.RepoVerifyRepoUnreachable, registry.RepoVerifyBranchMissing,
	}
	for _, v := range known {
		if !v.Valid() {
			t.Errorf("registered repo verify result %q reported invalid", v)
		}
	}
	if registry.RepoVerifyResult("PROBABLY_FINE").Valid() {
		t.Error("unregistered repo verify result reported valid")
	}
}

// 两个枚举各自封闭：仓库级的取值不得出现在路径级，反之亦然
// （design doc 2026-08-13 §3.3）。
//
// 一个枚举配两套「哪些值在这一层合法」的约定迟早会漂，而漂的方向是把
// 一句仓库级的话当成路径级的结论存下去：绑定上写着 AUTH_FAILED，读的人
// 会以为那是关于 policyPath 的判断，然后去改一个没有错的路径。这条用例
// 守的正是「拆成两个类型」这个决定本身 —— 只要哪一层的 Valid() 开始收
// 另一层的取值，两个枚举就退化成了一个。
//
// 共有的只有两个：NOT_VERIFIED 与 OK。
func TestRepoAndBindingVerdictsDoNotOverlapBeyondTheirSharedTwo(t *testing.T) {
	for _, shared := range []string{"NOT_VERIFIED", "OK"} {
		if !registry.RepoVerifyResult(shared).Valid() {
			t.Errorf("shared verdict %q is not valid at the repo level", shared)
		}
		if !registry.BindingVerifyResult(shared).Valid() {
			t.Errorf("shared verdict %q is not valid at the binding level", shared)
		}
	}

	// 仓库级独有的四个失败结论，路径级一律不认。
	for _, repoOnly := range []string{
		"CREDENTIAL_UNRESOLVED", "AUTH_FAILED", "REPO_UNREACHABLE", "BRANCH_MISSING",
	} {
		if registry.BindingVerifyResult(repoOnly).Valid() {
			t.Errorf("repo-level verdict %q is accepted by BindingVerifyResult.Valid(); "+
				"a repository-level failure stored on the binding reads as a claim about policyPath", repoOnly)
		}
	}

	// 路径级独有的那一个，仓库级同样不认。
	if registry.RepoVerifyResult("PATH_MISSING").Valid() {
		t.Error("binding-level verdict \"PATH_MISSING\" is accepted by RepoVerifyResult.Valid(); " +
			"the repo level never looks at policyPath")
	}
}
