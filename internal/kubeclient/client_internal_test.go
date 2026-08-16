package kubeclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// 拨号必须在判定之后。表里放的是同一批目标的各种承载形态：
// 只拦十进制点分写法是拦不住的（安全规范补充版 §26）。
//
// 全部用字面量地址，不触发任何 DNS 查询：Go 的解析器对字面量直接返回。
func TestGuardedDialRefusesBlockedDestinations(t *testing.T) {
	blocked := []struct {
		name    string
		address string
	}{
		{"GCP and AWS metadata endpoint", "169.254.169.254:6443"},
		{"IPv4 link-local", "169.254.10.20:6443"},
		{"IPv6 link-local", "[fe80::1]:6443"},
		{"Azure instance metadata", "168.63.129.16:6443"},
		{"Alibaba metadata endpoint", "100.100.100.200:6443"},
		{"AWS IPv6 metadata endpoint", "[fd00:ec2::254]:6443"},
		{"unspecified IPv4", "0.0.0.0:6443"},
		{"unspecified IPv6", "[::]:6443"},
		{"IPv4 multicast", "224.0.0.1:6443"},
		{"IPv4 broadcast", "255.255.255.255:6443"},

		// 4-in-6 包装。**这三条钉的是拨号这一层**：guardedDial 拿到什么
		// 就判什么、判完直拨同一个地址。LookupNetIP 对 network="ip"
		// 返回的本来就是 ::ffff: 形态，不只是显式写成这样的输入才有包装。
		// 判定本身是否拆包装由 checkIP 自己负责，见
		// destination_internal_test.go 的拒绝表 —— 两层各有各的用例，
		// 拿掉拨号前那一步 Unmap 时这三条仍绿（守卫下沉了），
		// 拿掉 checkIP 里那一步时两边一起红。
		{"IPv4-mapped IPv6 GCP metadata endpoint", "[::ffff:169.254.169.254]:6443"},
		{"IPv4-mapped IPv6 Azure metadata endpoint", "[::ffff:168.63.129.16]:6443"},
		{"IPv4-mapped IPv6 Alibaba metadata endpoint", "[::ffff:100.100.100.200]:6443"},

		// 带 zone 的写法。注意这两条钉的是"这种写法也拒绝"，钉不住
		// guardedDial 里的 WithZone("")：解析器返回的地址已经不带 zone，
		// 拿掉那一步这两条仍然绿 —— zone 现在由 checkIP 自己剥，见
		// TestCheckIPRefusesAZonedMetadataAddressOnItsOwn。
		{"zoned AWS IPv6 metadata endpoint", "[fd00:ec2::254%eth0]:6443"},
		{"zoned IPv6 link-local", "[fe80::1%eth0]:6443"},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			conn, err := guardedDial(ctx, "tcp", c.address)
			if conn != nil {
				_ = conn.Close()
				t.Fatalf("guardedDial(%s) returned a connection, want none", c.address)
			}
			if !errors.Is(err, ErrBlockedDestination) {
				t.Errorf("guardedDial(%s) = %v, want ErrBlockedDestination", c.address, err)
			}
		})
	}
}

// 不成形的地址同样拒绝，不是交给传输层去猜。
func TestGuardedDialRefusesMalformedAddresses(t *testing.T) {
	for _, address := range []string{"", "not-an-address", "10.0.0.1", "[::1]"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := guardedDial(ctx, "tcp", address)
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("guardedDial(%q) = %v, want ErrBlockedDestination", address, err)
		}
	}
}

// 放行的那一半也要证明：判定通过时确实拨得通，而不是一律拒绝。
// 主机名形式一并走一遍 —— 守卫自己解析、按解析结果判定、再直拨这个 IP，
// 判定与连接之间没有第二次解析（DNS rebinding 窗口，补充版 §24）。
func TestGuardedDialConnectsToAPermittedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}

	for _, address := range []string{ln.Addr().String(), net.JoinHostPort("localhost", port)} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := guardedDial(ctx, "tcp", address)
		cancel()
		if err != nil {
			t.Errorf("guardedDial(%s) = %v, want a connection", address, err)
			continue
		}
		_ = conn.Close()
	}
}

// 只证明 checkIP 会拒绝还不够，还要证明构造出来的配置确实带着它 ——
// 把 newFromConfig 里的 cfg.Dial 摘掉，函数级用例仍然全绿。
func TestNewFromConfigPinsTheDialerToTheGuard(t *testing.T) {
	cfg := &rest.Config{Host: "https://apiserver.example.internal:6443"}
	if _, err := newFromConfig(cfg); err != nil {
		t.Fatalf("newFromConfig() = %v", err)
	}

	if cfg.Dial == nil {
		t.Fatal("cfg.Dial is nil, want the address guard")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := cfg.Dial(ctx, "tcp", "169.254.169.254:6443")
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, ErrBlockedDestination) {
		t.Errorf("cfg.Dial(metadata endpoint) = %v, want ErrBlockedDestination", err)
	}

	if cfg.UserAgent != userAgent {
		t.Errorf("cfg.UserAgent = %q, want %q", cfg.UserAgent, userAgent)
	}
	if cfg.Timeout != requestTimeout {
		t.Errorf("cfg.Timeout = %v, want %v", cfg.Timeout, requestTimeout)
	}
}

// 再往外一层：证明 client-go 真的走这个 Dial，而不只是把它存了下来。
// 需要一个活的监听端口 —— 没有它，"配置带着守卫"与"请求确实经过守卫"
// 之间的那一段就只能靠读代码相信。
func TestClientGoIssuesItsRequestsThroughTheGuard(t *testing.T) {
	var reached atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"kind":"NamespaceList","apiVersion":"v1","metadata":{},"items":[]}`)
	}))
	defer srv.Close()

	// 放行的一侧：httptest 监听在回环地址上，守卫允许它，请求必须到达。
	allowed, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("newFromConfig(test server) = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := allowed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("List() against the test server = %v, want success", err)
	}
	if !reached.Load() {
		t.Fatal("the test server was never reached, the rest of this test would prove nothing")
	}

	// 拒绝的一侧：同一条路径，目标换成云元数据端点。
	blocked, err := newFromConfig(&rest.Config{Host: "http://169.254.169.254:6443"})
	if err != nil {
		t.Fatalf("newFromConfig(metadata endpoint) = %v", err)
	}
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer blockedCancel()
	if _, err := blocked.CoreV1().Namespaces().List(blockedCtx, metav1.ListOptions{}); !errors.Is(err, ErrBlockedDestination) {
		t.Errorf("List() against the metadata endpoint = %v, want ErrBlockedDestination", err)
	}
}

func TestNewRejectsAMalformedKubeconfig(t *testing.T) {
	for _, kubeconfig := range [][]byte{nil, []byte("not a kubeconfig"), []byte("{")} {
		if _, err := New(kubeconfig); err == nil {
			t.Errorf("New(%q) = nil error, want a parse failure", kubeconfig)
		}
	}
}

func TestNewBuildsAClientFromAValidKubeconfig(t *testing.T) {
	kubeconfig := []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://apiserver.example.internal:6443
contexts:
- name: c
  context:
    cluster: c
    user: u
current-context: c
users:
- name: u
  user:
    token: t
`)
	client, err := New(kubeconfig)
	if err != nil {
		t.Fatalf("New(valid kubeconfig) = %v", err)
	}
	if client == nil {
		t.Fatal("New(valid kubeconfig) returned a nil client")
	}
}
