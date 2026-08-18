package gitverify_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/imkerbos/Distill/internal/registry"
)

// commitInto 在 url 指向的裸仓库上再做一次提交，返回新的 HEAD SHA。
//
// 走真实仓库而不是替身：漂移检测要读两棵树并比对，而两棵树来自哪里、
// 浅克隆够不够，恰恰是这份实现最容易错的地方。
func commitInto(t *testing.T, url, path, content string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	repo, err := git.PlainClone(work, false, &git.CloneOptions{URL: url})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	full := filepath.Join(work, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := w.Add("."); err != nil {
		t.Fatalf("add: %v", err)
	}
	h, err := w.Commit("change", &git.CommitOptions{
		Author: &object.Signature{Name: "someone", Email: "s@example.com", When: time.Unix(1, 0).UTC()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := repo.Push(&git.PushOptions{}); err != nil {
		t.Fatalf("push: %v", err)
	}
	return h.String()
}

// headOf 返回裸仓库当前分支的 HEAD SHA。
func headOf(t *testing.T, url string) string {
	t.Helper()
	repo, err := git.PlainOpen(strings.TrimPrefix(url, "file://"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return head.Hash().String()
}

func bindingFor(url, branch, path, anchor string) (registry.GitRepo, string, string) {
	return registry.GitRepo{URL: url, Branch: branch}, path, anchor
}

func TestDriftInSyncWhenNothingChanged(t *testing.T) {
	url := newBareTestRepo(t)
	anchor := headOf(t, url)
	r, p, a := bindingFor(url, "master", "policies/prod", anchor)

	got := newVerifier(t).Drift(context.Background(), r, p, a)
	if got != registry.DriftInSync {
		t.Errorf("Drift() = %q, want %q", got, registry.DriftInSync)
	}
}

// **分支往前走不等于漂移。** 别人在别的路径上提交是常态，把它报成漂移
// 会让这个信号每天都在响，而一个每天都响的信号会被整体忽略。
func TestDriftIgnoresCommitsOutsideThePolicyPath(t *testing.T) {
	url := newBareTestRepo(t)
	anchor := headOf(t, url)
	commitInto(t, url, "README.md", "unrelated\n")
	if headOf(t, url) == anchor {
		t.Fatal("the branch did not move; this test would prove nothing")
	}
	r, p, a := bindingFor(url, "master", "policies/prod", anchor)

	if got := newVerifier(t).Drift(context.Background(), r, p, a); got != registry.DriftInSync {
		t.Errorf("Drift() = %q, want %q — 分支动了，但我们那条路径没变", got, registry.DriftInSync)
	}
}

func TestDriftDetectsAChangeUnderThePolicyPath(t *testing.T) {
	url := newBareTestRepo(t)
	anchor := headOf(t, url)
	commitInto(t, url, "policies/prod/np.yaml", "kind: NetworkPolicy\n# edited by hand\n")
	r, p, a := bindingFor(url, "master", "policies/prod", anchor)

	if got := newVerifier(t).Drift(context.Background(), r, p, a); got != registry.DriftDrifted {
		t.Errorf("Drift() = %q, want %q", got, registry.DriftDrifted)
	}
}

func TestDriftReportsNeverWrittenWithoutAnAnchor(t *testing.T) {
	url := newBareTestRepo(t)
	r, p, a := bindingFor(url, "master", "policies/prod", "")

	if got := newVerifier(t).Drift(context.Background(), r, p, a); got != registry.DriftNeverWritten {
		t.Errorf("Drift() = %q, want %q", got, registry.DriftNeverWritten)
	}
}

// 锚点提交在仓库里找不到了 = 有人改写过历史。
//
// 与 DRIFTED 分开：那条历史里我们那次提交连同它的审计线索一起没了，
// 处置是去查谁 force push 了，不是重推一次。
func TestDriftReportsAMissingAnchorSeparately(t *testing.T) {
	url := newBareTestRepo(t)
	r, p, a := bindingFor(url, "master", "policies/prod",
		"0123456789abcdef0123456789abcdef01234567")

	if got := newVerifier(t).Drift(context.Background(), r, p, a); got != registry.DriftAnchorMissing {
		t.Errorf("Drift() = %q, want %q", got, registry.DriftAnchorMissing)
	}
}

// **够不到仓库时绝不能答"一致"。**
//
// 一次网络抖动读成一致，操作者就以为下发的东西还在，而它可能早被人删了。
// 失败方向必须朝"不知道"（安全规范 §49）。
func TestDriftAnswersUnknownWhenTheRepoCannotBeRead(t *testing.T) {
	r, p, a := bindingFor("file://"+filepath.Join(t.TempDir(), "nowhere"), "master",
		"policies/prod", "0123456789abcdef0123456789abcdef01234567")

	got := newVerifier(t).Drift(context.Background(), r, p, a)
	if got == registry.DriftInSync {
		t.Fatal("an unreachable repository was reported as in sync")
	}
	if got != registry.DriftUnknown {
		t.Errorf("Drift() = %q, want %q", got, registry.DriftUnknown)
	}
}
