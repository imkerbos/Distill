package secrets

import "testing"

// TestPathWithinDirRejectsEscape 直接测 pathWithinDir 本身，不经过
// ValidateRef。ValidateRef 今天已经把这些 ref 全部挡在外面，所以走
// Resolve() 的黑盒测试测不到这个函数——白盒测试的意义就在于绕开
// ValidateRef，单独确认 pathWithinDir 在字符集校验失效时仍能兜底。
//
// 不含 "/etc/passwd" 这类绝对路径形态的 ref：filepath.Join 不会把
// 非首个参数的前导 "/" 当成绝对路径处理，Join(dir, "/etc/passwd")
// 就是 dir/etc/passwd，本来就落在 dir 之内——这条向量是 ValidateRef
// （禁止 `/`）挡住的，不是 pathWithinDir 该管的范围，已经在
// TestDirResolverRefusesToEscapeItsDirectory 里覆盖。这里只测
// pathWithinDir 真正要防的向量：`..` 段。
func TestPathWithinDirRejectsEscape(t *testing.T) {
	dir := "/var/lib/distill/secrets"
	escapes := []string{"../outside", "..", "sub/../../outside"}
	for _, ref := range escapes {
		if _, ok := pathWithinDir(dir, ref); ok {
			t.Errorf("pathWithinDir(%q, %q) = true, want false", dir, ref)
		}
	}

	if _, ok := pathWithinDir(dir, "prod-asia-1"); !ok {
		t.Errorf("pathWithinDir(%q, %q) = false, want true", dir, "prod-asia-1")
	}
}
