package gitwrite

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// 推送永不强制。
//
// 这一条只能在这里断言：远端已存在的分支先被 checkTargetAbsent 挡下了，因此
// 「带不带 force」在 file:// 的行为上看不出差别 —— 一个加在 pushOptions 里的
// Force: true 不会让任何端到端用例变红。两层守卫各自漏的东西不一样（见
// checkTargetAbsent 的注释），所以两层都要有自己的断言。
func TestPushOptionsAreNeverForced(t *testing.T) {
	target := plumbing.NewBranchReferenceName("distill/prod-asia-1-20260814T093000Z")
	opts := pushOptions(target, nil)

	if opts.Force {
		t.Error("pushOptions().Force = true —— 强制推送会覆盖远端已有的分支")
	}
	if opts.ForceWithLease != nil {
		t.Error("pushOptions().ForceWithLease is set —— 带租约的强制推送仍然是强制推送")
	}
	if opts.Prune {
		t.Error("pushOptions().Prune = true —— 本包从不删除任何东西")
	}
	if len(opts.RefSpecs) != 1 {
		t.Fatalf("pushOptions().RefSpecs = %v, want exactly one", opts.RefSpecs)
	}
	spec := opts.RefSpecs[0]
	if spec.IsForceUpdate() {
		t.Errorf("refspec %q starts with + —— 那个加号就是强制推送", spec)
	}
	if spec.IsDelete() {
		t.Errorf("refspec %q deletes a remote ref", spec)
	}
	// 显式写死目标，不依赖默认 refspec：默认会推 refs/heads/*，把基底分支
	// 一起推出去。
	if want := target.String() + ":" + target.String(); spec.String() != want {
		t.Errorf("refspec = %q, want %q", spec, want)
	}
}
