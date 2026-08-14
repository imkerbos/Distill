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

	"github.com/imkerbos/Distill/internal/gitssh"
	"github.com/imkerbos/Distill/internal/registry"
)

// gitssh 那边证明的是 Transport.Auth **造出来的**那个认证方法带着守卫；
// 它证明不了 cloneBranch 会去调它。这两件事必须各自回答一遍：cloneBranch 是
// 平台唯一的出站拨号路径，而 Transport.Auth 是唯一给这条链路挂上「钉死的
// host key」与「目的地址判定」的地方 —— 把那一行换成裸的
// gogitssh.NewPublicKeys，两样守卫一起消失，而全部黑盒测试仍然全绿：
// 它们走 file://，那条传输永远不协商 SSH。
//
// 守卫搬进 gitssh 之后这条用例更要留在这里：跨包之后「调用方还在调用它」
// 这件事在 gitssh 内部根本表达不出来，而这正是本项目反复出现的那类缺陷 ——
// 守卫被测到，没有东西证明调用点还在经过它。
//
// 这里跑一次**真实的 SSH 握手**：回环地址上起一个真的服务端，拨号成功、
// 服务端交出 host key、客户端的 HostKeyCallback 因此真的被调用。此刻整个
// 进程里唯一能产生 gitssh.ErrBlockedDestination 的东西是 gitssh 的
// guardDestination，而它只在 Transport.Auth 里挂上去 —— 于是「这次拨号被
// 本平台的目的地址判定拒掉」这个断言，等价于「cloneBranch 交给 go-git 的
// 认证方法确实出自 Transport.Auth」。绕开它时 go-git 换上自己的默认回调，
// 错误不再是这个哨兵，断言当场变红。
//
// 断的是哨兵而不是「握手失败」：任何一次失败的握手都会返回某个错误，
// 按「有没有出错」判等于什么都没判。
//
// **它证明不到的那一格：** 钉死的 host key 清单在这条用例里从未被比对 ——
// 目的地址判定先跑，握手在比对之前就断了（顺序是刻意的，见
// gitssh/destination.go）。清单本身由 gitssh 的白盒用例守。要连这一格一起证明，需要一次落在公网地址
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
	if _, err := v.cloneBranch(context.Background(), repo); !errors.Is(err, gitssh.ErrBlockedDestination) {
		t.Fatalf("cloneBranch() = %v, want the destination guard's refusal: "+
			"the outbound dial did not go through the auth method Transport.Auth builds", err)
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
