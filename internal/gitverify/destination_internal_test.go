package gitverify

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/imkerbos/Distill/internal/registry"
)

// 目的地址判定必须默认拒绝。表里放的是各种等价写法：只拦十进制点分的
// 127.0.0.1 是拦不住的（补充版 §26）。
func TestCheckDestinationRefusesInternalAddresses(t *testing.T) {
	blocked := []struct {
		name string
		addr string
	}{
		{"IPv4 loopback", "127.0.0.1:22"},
		{"IPv4 loopback elsewhere in 127.0.0.0/8", "127.1.2.3:22"},
		{"IPv6 loopback", "[::1]:22"},
		{"IPv4-mapped IPv6 loopback", "[::ffff:127.0.0.1]:22"},
		{"RFC1918 ten", "10.0.0.5:22"},
		{"RFC1918 172.16/12", "172.20.1.1:22"},
		{"RFC1918 192.168/16", "192.168.1.1:22"},
		{"IPv4-mapped IPv6 RFC1918", "[::ffff:10.0.0.5]:22"},
		{"link-local unicast", "169.254.10.20:22"},
		{"GCP and AWS metadata endpoint", "169.254.169.254:22"},
		{"IPv4-mapped IPv6 metadata endpoint", "[::ffff:169.254.169.254]:22"},
		{"IPv6 link-local", "[fe80::1]:22"},
		{"IPv6 unique-local", "[fd00::1]:22"},
		{"AWS IPv6 metadata endpoint", "[fd00:ec2::254]:22"},
		{"unspecified IPv4", "0.0.0.0:22"},
		{"unspecified IPv6", "[::]:22"},
		{"IPv4 multicast", "224.0.0.1:22"},
		{"IPv6 multicast", "[ff02::1]:22"},
		{"IPv6 interface-local multicast", "[ff01::1]:22"},
		{"IPv4 broadcast", "255.255.255.255:22"},
		{"Azure metadata endpoint", "168.63.129.16:22"},
		{"Alibaba metadata endpoint", "100.100.100.200:22"},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if err := checkDestination(tcpAddr(t, c.addr)); !errors.Is(err, errBlockedDestination) {
				t.Errorf("checkDestination(%s) = %v, want errBlockedDestination", c.addr, err)
			}
			// 同一个地址换成非 *net.TCPAddr 的实现也必须拒绝：判定不能
			// 依赖 net.Addr 的具体类型。
			if err := checkDestination(stringAddr(c.addr)); !errors.Is(err, errBlockedDestination) {
				t.Errorf("checkDestination(stringAddr %s) = %v, want errBlockedDestination", c.addr, err)
			}
		})
	}

	allowed := []struct {
		name string
		addr string
	}{
		{"public IPv4", "140.82.121.4:22"},
		{"public IPv6", "[2606:50c0:8000::153]:22"},
		{"IPv4-mapped IPv6 public address", "[::ffff:140.82.121.4]:22"},
	}
	for _, c := range allowed {
		t.Run(c.name, func(t *testing.T) {
			if err := checkDestination(tcpAddr(t, c.addr)); err != nil {
				t.Errorf("checkDestination(%s) = %v, want nil", c.addr, err)
			}
		})
	}
}

// 带 zone 的链路本地地址两种承载形态都必须拒绝：zone 会让部分 Is* 判定
// 落进另一条分支（补充版 §26）。
func TestCheckDestinationRefusesZonedLinkLocal(t *testing.T) {
	zoned := []net.Addr{
		&net.TCPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0", Port: 22},
		stringAddr("[fe80::1%eth0]:22"),
	}
	for _, addr := range zoned {
		if err := checkDestination(addr); !errors.Is(err, errBlockedDestination) {
			t.Errorf("checkDestination(%v) = %v, want errBlockedDestination", addr, err)
		}
	}
}

// 认不出形态的地址必须拒绝，不是放行。
func TestCheckDestinationRefusesUnrecognizableAddresses(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
	}{
		{"nil address", nil},
		{"unix socket", &net.UnixAddr{Name: "/tmp/ssh.sock", Net: "unix"}},
		{"address that is not host:port", stringAddr("not-an-address")},
		{"host:port that is not an IP", stringAddr("metadata.google.internal:80")},
		{"TCP address with a malformed IP", &net.TCPAddr{IP: net.IP{1, 2, 3}, Port: 22}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkDestination(c.addr); !errors.Is(err, errBlockedDestination) {
				t.Errorf("checkDestination(%v) = %v, want errBlockedDestination", c.addr, err)
			}
		})
	}
}

// 被拒绝的目的地址必须落在封闭枚举的 REPO_UNREACHABLE 上，且结论里不得
// 带任何地址信息（§19、§22）。
func TestClassifyMapsBlockedDestinationToRepoUnreachable(t *testing.T) {
	if got := Classify(errBlockedDestination); got != registry.RepoVerifyRepoUnreachable {
		t.Errorf("Classify(errBlockedDestination) = %q, want %q", got, registry.RepoVerifyRepoUnreachable)
	}
	// x/crypto/ssh 与 go-git 都会在外面包一层，包完仍要落在同一档。
	wrapped := fmt.Errorf("ssh: handshake failed: %w", errBlockedDestination)
	if got := Classify(wrapped); got != registry.RepoVerifyRepoUnreachable {
		t.Errorf("Classify(wrapped) = %q, want %q", got, registry.RepoVerifyRepoUnreachable)
	}
}

// 光证明判定函数会拒绝还不够 —— 还要证明交给 go-git 的那个认证方法确实
// 带着它。这与 host key 回调是同一个理由：file:// 传输不协商 SSH，把
// sshAuth 里的 guardDestination 摘掉，黑盒用例仍然全绿。
func TestSSHAuthCarriesTheDestinationGuard(t *testing.T) {
	pinned := newHostKey(t)
	known := []byte("git.example.com " + string(cryptossh.MarshalAuthorizedKey(pinned)))
	v, err := New(nilResolver{}, known, time.Second)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	auth, err := v.sshAuth(newPrivateKeyPEM(t))
	if err != nil {
		t.Fatalf("sshAuth() = %v", err)
	}

	// 主机名与 host key 都是配置里认可的那一组，唯一不对的是对端地址。
	// 没有目的地址判定时这次调用会返回 nil。
	for _, addr := range []string{"127.0.0.1:22", "169.254.169.254:22", "10.0.0.5:22", "[::ffff:127.0.0.1]:22"} {
		err := auth.HostKeyCallback("git.example.com:22", tcpAddr(t, addr), pinned)
		if !errors.Is(err, errBlockedDestination) {
			t.Errorf("auth.HostKeyCallback(remote %s) = %v, want errBlockedDestination", addr, err)
		}
	}

	// 目的地址没问题时必须继续走 host key 校验，而不是一律放行或一律拒绝。
	if err := auth.HostKeyCallback("git.example.com:22", tcpAddr(t, "140.82.121.4:22"), pinned); err != nil {
		t.Errorf("auth.HostKeyCallback(public remote, pinned key) = %v, want nil", err)
	}
	if err := auth.HostKeyCallback("git.example.com:22", tcpAddr(t, "140.82.121.4:22"), newHostKey(t)); err == nil {
		t.Error("auth.HostKeyCallback(public remote, unknown key) = nil, want refusal")
	}
}

func tcpAddr(t *testing.T, addr string) *net.TCPAddr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}
	return a
}

// stringAddr 是一个只会报出字符串的 net.Addr —— 传输层给的未必是
// *net.TCPAddr，判定不得依赖具体类型。
type stringAddr string

func (stringAddr) Network() string  { return "tcp" }
func (a stringAddr) String() string { return string(a) }
