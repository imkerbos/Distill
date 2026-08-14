package gitverify

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"net"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/imkerbos/Distill/internal/registry"
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

	// 用公网地址：sshAuth 返回的回调外层还包着目的地址判定，内网地址会在
	// 比对 host key 之前就被拒掉，这条用例要验的不是那一层。
	addr := &net.TCPAddr{IP: net.IPv4(140, 82, 121, 4), Port: 22}
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

// 上一条证明的是 sshAuth **造出来的**那个认证方法带着守卫；它证明不了
// cloneBranch 会去调它。这两件事必须各自回答一遍：cloneBranch 是平台唯一的
// 出站拨号路径，而 sshAuth 是唯一给这条链路挂上「钉死的 host key」与「目的
// 地址判定」的地方 —— 把那一行换成裸的 gitssh.NewPublicKeys，两样守卫一起
// 消失，而全部黑盒测试仍然全绿：它们走 file://，那条传输永远不协商 SSH。
//
// 这里跑一次**真实的 SSH 握手**：回环地址上起一个真的服务端，拨号成功、
// 服务端交出 host key、客户端的 HostKeyCallback 因此真的被调用。此刻整个
// 进程里唯一能产生 errBlockedDestination 的东西是 guardDestination，而它
// 只在 sshAuth 里挂上去 —— 于是「这次拨号被本平台的目的地址判定拒掉」这个
// 断言，等价于「cloneBranch 交给 go-git 的认证方法确实出自 sshAuth」。
// 绕开 sshAuth 时 go-git 换上它自己的默认回调，错误不再是这个哨兵，
// 断言当场变红。
//
// 断的是哨兵而不是「握手失败」：任何一次失败的握手都会返回某个错误，
// 按「有没有出错」判等于什么都没判。
//
// **它证明不到的那一格：** 钉死的 host key 清单在这条用例里从未被比对 ——
// 目的地址判定先跑，握手在比对之前就断了（顺序是刻意的，见 destination.go）。
// 清单本身由上面两条白盒用例守。要连这一格一起证明，需要一次落在公网地址
// 上的真实握手，那不属于单元测试能给的东西。
func TestCloneBranchDialsThroughTheGuardedAuth(t *testing.T) {
	addr, hostKey := startHandshakeOnlySSHListener(t)

	// host key 钉进清单：这条用例里 host key 那一层是**放行**的，于是唯一
	// 还能拦住这次拨号的东西就是目的地址判定。
	known := []byte(knownhosts.Normalize(addr) + " " + string(cryptossh.MarshalAuthorizedKey(hostKey)))
	v, err := New(pemResolver{key: newPrivateKeyPEM(t)}, known, 10*time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	repo := registry.GitRepo{URL: "ssh://git@" + addr + "/policies.git", Branch: "master"}
	if _, err := v.cloneBranch(context.Background(), repo); !errors.Is(err, errBlockedDestination) {
		t.Fatalf("cloneBranch() = %v, want the destination guard's refusal: "+
			"the outbound dial did not go through the auth method sshAuth builds", err)
	}
}

// startHandshakeOnlySSHListener 在回环地址上起一个只做密钥交换的 SSH
// 服务端，返回它的地址与 host key。
//
// 客户端要走到 HostKeyCallback，服务端就必须真的把 host key 交出来 ——
// 一个只 Accept 就关掉的监听给不出这一步，错误会停在「握手失败：EOF」，
// 那证明不了目的地址判定跑过。
func startHandshakeOnlySSHListener(t *testing.T) (string, cryptossh.PublicKey) {
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
				sc, _, reqs, err := cryptossh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer func() { _ = sc.Close() }()
				cryptossh.DiscardRequests(reqs)
			}()
		}
	}()

	return ln.Addr().String(), signer.PublicKey()
}

// pemResolver 代替 Secret Manager，返回一把当场生成的私钥。
type pemResolver struct{ key []byte }

func (p pemResolver) Resolve(context.Context, string) ([]byte, error) { return p.key, nil }

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
