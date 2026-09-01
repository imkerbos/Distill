package gitssh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
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
			if err := checkDestination(tcpAddr(t, c.addr), nil); !errors.Is(err, ErrBlockedDestination) {
				t.Errorf("checkDestination(%s, nil) = %v, want ErrBlockedDestination", c.addr, err)
			}
			// 同一个地址换成非 *net.TCPAddr 的实现也必须拒绝：判定不能
			// 依赖 net.Addr 的具体类型。
			if err := checkDestination(stringAddr(c.addr), nil); !errors.Is(err, ErrBlockedDestination) {
				t.Errorf("checkDestination(stringAddr %s, nil) = %v, want ErrBlockedDestination", c.addr, err)
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
			if err := checkDestination(tcpAddr(t, c.addr), nil); err != nil {
				t.Errorf("checkDestination(%s, nil) = %v, want nil", c.addr, err)
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
		if err := checkDestination(addr, nil); !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("checkDestination(%v, nil) = %v, want ErrBlockedDestination", addr, err)
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
			if err := checkDestination(c.addr, nil); !errors.Is(err, ErrBlockedDestination) {
				t.Errorf("checkDestination(%v, nil) = %v, want ErrBlockedDestination", c.addr, err)
			}
		})
	}
}

// 光证明判定函数会拒绝还不够 —— 还要证明交给 go-git 的那个认证方法确实
// 带着它。这与 host key 回调是同一个理由：file:// 传输不协商 SSH，把
// Auth 里的 guardDestination 摘掉，黑盒用例仍然全绿。
func TestAuthCarriesTheDestinationGuard(t *testing.T) {
	pinned := newHostKey(t)
	known := []byte("git.example.com " + string(cryptossh.MarshalAuthorizedKey(pinned)))
	tr, err := New(pemResolver{key: newPrivateKeyPEM(t)}, known, time.Second, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	auth, err := tr.Auth(context.Background(), "deploy-key")
	if err != nil {
		t.Fatalf("Auth() = %v", err)
	}

	// 主机名与 host key 都是配置里认可的那一组，唯一不对的是对端地址。
	// 没有目的地址判定时这次调用会返回 nil。
	for _, addr := range []string{"127.0.0.1:22", "169.254.169.254:22", "10.0.0.5:22", "[::ffff:127.0.0.1]:22"} {
		err := auth.HostKeyCallback("git.example.com:22", tcpAddr(t, addr), pinned)
		if !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("auth.HostKeyCallback(remote %s) = %v, want ErrBlockedDestination", addr, err)
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

// mustPrefixes 是测试里写网段清单的简写。
func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("ParsePrefix(%s): %v", c, err)
		}
		out = append(out, p)
	}
	return out
}

// 登记过的内网网段可以出站 —— 这是这份清单存在的理由。
//
// UAT 的形状：gitlab-devops 解析到 10.170.1.11，而策略仓库在内网是正常
// 部署形态（design doc 2026-09-01 §1）。
func TestAllowlistPermitsARegisteredPrivateRange(t *testing.T) {
	allowed := mustPrefixes(t, "10.170.0.0/16")
	if err := checkDestination(tcpAddr(t, "10.170.1.11:22"), allowed); err != nil {
		t.Errorf("登记过的网段仍被拒: %v", err)
	}
	// 没登记的那一段照旧拒：清单是"这几段"，不是"内网都行"。
	if err := checkDestination(tcpAddr(t, "10.171.1.11:22"), allowed); !errors.Is(err, ErrBlockedDestination) {
		t.Errorf("没登记的网段被放行了: %v", err)
	}
}

// **清单改不了第一层。** 这是整个设计的支点。
//
// 云元数据地址一旦可以被清单放行，平台就成了一把偷取实例凭据的钥匙；
// 回环一旦可以放行，平台就成了本机服务的探测器。设置层的校验已经挡住
// 这些网段（它们都不是私有地址），这一条守的是**即便有人绕过校验把它们
// 写进库**，拨号仍然被拒（§3.1）。
func TestAllowlistCannotUnblockTheAbsoluteDenials(t *testing.T) {
	for _, tc := range []struct{ name, cidr, addr string }{
		{"回环", "127.0.0.0/8", "127.0.0.1:22"},
		{"链路本地（含 169.254.169.254）", "169.254.0.0/16", "169.254.169.254:22"},
		{"Azure 元数据", "168.63.129.16/32", "168.63.129.16:22"},
		{"阿里云元数据所在段", "100.64.0.0/10", "100.100.100.200:22"},
		{"整个 IPv4", "0.0.0.0/0", "127.0.0.1:22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed := mustPrefixes(t, tc.cidr)
			if err := checkDestination(tcpAddr(t, tc.addr), allowed); !errors.Is(err, ErrBlockedDestination) {
				t.Errorf("清单放行了 %s —— 这一层不该被清单改动: %v", tc.addr, err)
			}
		})
	}
}

// 公网照旧放行，清单不影响它。
func TestAllowlistDoesNotAffectPublicAddresses(t *testing.T) {
	allowed := mustPrefixes(t, "10.170.0.0/16")
	if err := checkDestination(tcpAddr(t, "93.184.216.34:22"), allowed); err != nil {
		t.Errorf("公网地址被拒: %v", err)
	}
}

// **空清单等于引入它之前的行为。** 一次升级不该悄悄改变任何现有部署的出站面。
func TestAnEmptyAllowlistKeepsPrivateAddressesBlocked(t *testing.T) {
	for _, addr := range []string{"10.170.1.11:22", "192.168.1.1:22", "172.16.0.1:22"} {
		if err := checkDestination(tcpAddr(t, addr), nil); !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("空清单下 %s 被放行了: %v", addr, err)
		}
	}
}
