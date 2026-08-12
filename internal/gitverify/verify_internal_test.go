package gitverify

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"net"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

// host key 校验回调在 file:// 传输上不参与，黑盒测试碰不到它 —— 一个
// 永远返回 nil 的回调可以让整套黑盒测试全绿。它是这条链路上唯一挡住
// 中间人的东西，所以在这里白盒测。
func TestHostKeyCallbackAcceptsOnlyConfiguredKeys(t *testing.T) {
	pinned := newHostKey(t)
	other := newHostKey(t)

	cb, err := hostKeyCallback([]byte("git.example.com " + string(cryptossh.MarshalAuthorizedKey(pinned))))
	if err != nil {
		t.Fatalf("hostKeyCallback() = %v", err)
	}

	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 22}
	cases := []struct {
		name     string
		hostname string
		key      cryptossh.PublicKey
		wantOK   bool
	}{
		{"configured host with the pinned key", "git.example.com:22", pinned, true},
		{"same host written without the default port", "git.example.com", pinned, true},
		{"configured host presenting a different key", "git.example.com:22", other, false},
		{"host that was never configured", "git.elsewhere.example.com:22", pinned, false},
		{"configured host reached on another port", "git.example.com:2222", pinned, false},
	}
	for _, c := range cases {
		err := cb(c.hostname, addr, c.key)
		if gotOK := err == nil; gotOK != c.wantOK {
			t.Errorf("%s: callback err = %v, want ok=%v", c.name, err, c.wantOK)
		}
	}
}

// 光证明回调本身会拒绝还不够 —— 还要证明交给 go-git 的那个认证方法
// 确实带着它。file:// 传输不协商 SSH，把 auth.HostKeyCallback 换成
// InsecureIgnoreHostKey 时整套黑盒测试仍然全绿，中间人就是这样进来的。
func TestSSHAuthCarriesThePinnedHostKeyCallback(t *testing.T) {
	pinned := newHostKey(t)
	v, err := New(nilResolver{}, []byte("git.example.com "+string(cryptossh.MarshalAuthorizedKey(pinned))), time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	auth, err := v.sshAuth(newPrivateKeyPEM(t))
	if err != nil {
		t.Fatalf("sshAuth() = %v", err)
	}
	if auth.HostKeyCallback == nil {
		t.Fatal("sshAuth() left HostKeyCallback nil: go-git would fall back to its own default")
	}

	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 22}
	if err := auth.HostKeyCallback("git.elsewhere.example.com:22", addr, pinned); err == nil {
		t.Error("auth accepted a host that is not in the configured known-hosts data")
	}
	if err := auth.HostKeyCallback("git.example.com:22", addr, pinned); err != nil {
		t.Errorf("auth rejected the configured host with its pinned key: %v", err)
	}
	if err := auth.HostKeyCallback("git.example.com:22", addr, newHostKey(t)); err == nil {
		t.Error("auth accepted the configured host presenting a different key")
	}
}

type nilResolver struct{}

func (nilResolver) Resolve(context.Context, string) ([]byte, error) { return nil, nil }

func newPrivateKeyPEM(t *testing.T) []byte {
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

func newHostKey(t *testing.T) cryptossh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	key, err := cryptossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap host key: %v", err)
	}
	return key
}
