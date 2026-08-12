package secrets_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/imkerbos/Distill/internal/secrets"
)

func TestDirResolverReadsByRef(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prod-asia-1"), []byte("KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := secrets.NewDirResolver(dir).Resolve(context.Background(), "prod-asia-1")
	if err != nil || string(got) != "KEY" {
		t.Fatalf("Resolve() = %q, %v; want \"KEY\", nil", got, err)
	}
}

func TestDirResolverDistinguishesMissingFromBroken(t *testing.T) {
	r := secrets.NewDirResolver(t.TempDir())
	_, err := r.Resolve(context.Background(), "absent")
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Resolve(absent) = %v, want ErrNotFound", err)
	}
}

// 这是黑盒测试，走的是公开的 Resolve()。它证明的是 ValidateRef
// 拒绝了这些穿越形态的 ref——今天这些 ref 全部含 `/` 或 `.`，
// 在到达路径拼接之前就被字符集校验挡掉了。它不证明路径拼接后的
// 目录包含性检查本身管用；那部分由 pathWithinDir 的白盒测试
// （dir_internal_test.go）单独覆盖。
func TestDirResolverRefusesToEscapeItsDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"../outside", "..", "/etc/passwd", "sub/../../outside"} {
		got, err := secrets.NewDirResolver(dir).Resolve(context.Background(), ref)
		if err == nil {
			t.Errorf("Resolve(%q) = %q, nil; want error", ref, got)
		}
	}
}
