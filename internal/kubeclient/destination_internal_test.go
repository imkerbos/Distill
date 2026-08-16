package kubeclient

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// 允许拨号的地址表。**这一半和拒绝的那一半同样重要。**
//
// 本判定刻意比 internal/gitssh 宽：私有 GKE 集群的 apiserver 端点本来就在
// RFC1918 里，照搬 Git 远端那套"拒绝一切私有地址"会让采集器连不上正常的
// 生产集群，于是这条守卫会被整体关掉，等于什么都没守。
// 把这些地址钉成必须放行，后来者就不能在没有一条测试反对的情况下
// 把它"收紧"回没用的状态。
func TestCheckIPAllowsTheAddressesAPrivateClusterActuallyUses(t *testing.T) {
	allowed := []struct {
		name string
		ip   string
	}{
		{"RFC1918 ten, the usual private GKE endpoint", "10.0.0.2"},
		{"RFC1918 172.16/12", "172.20.1.1"},
		{"RFC1918 192.168/16", "192.168.1.1"},
		{"carrier-grade NAT range just below the blocked one", "100.63.255.255"},
		{"the address right above the blocked 100.64/10", "100.128.0.1"},
		{"public IPv4, the public GKE endpoint", "34.120.1.5"},
		{"public IPv6", "2600:1901::1"},
		{"IPv6 unique-local", "fd00::1"},
		{"IPv6 unique-local next to the AWS metadata address", "fd00:ec2::255"},
		{"IPv4 loopback, a local proxy or a kind cluster", "127.0.0.1"},
		{"IPv6 loopback", "::1"},
	}
	for _, c := range allowed {
		t.Run(c.name, func(t *testing.T) {
			if err := checkIP(netip.MustParseAddr(c.ip)); err != nil {
				t.Errorf("checkIP(%s) = %v, want nil", c.ip, err)
			}
		})
	}
}

// 只拦真正没有正当用途的目标：云元数据端点与链路本地地址。
// 一个 apiserver 永远不会在这些地址上。
func TestCheckIPRefusesMetadataAndLinkLocal(t *testing.T) {
	blocked := []struct {
		name string
		ip   string
	}{
		{"GCP, AWS and OpenStack metadata endpoint", "169.254.169.254"},
		{"IPv4 link-local elsewhere in 169.254/16", "169.254.10.20"},
		{"IPv6 link-local", "fe80::1"},
		{"IPv6 link-local at the top of fe80::/10", "febf::1"},
		{"Azure instance metadata, globally routable", "168.63.129.16"},
		{"Alibaba metadata endpoint", "100.100.100.200"},
		{"bottom of the RFC 6598 shared range", "100.64.0.0"},
		{"top of the RFC 6598 shared range", "100.127.255.255"},
		{"AWS IPv6 metadata endpoint", "fd00:ec2::254"},
		{"unspecified IPv4", "0.0.0.0"},
		{"unspecified IPv6", "::"},
		{"IPv4 multicast", "224.0.0.1"},
		{"IPv6 multicast", "ff02::1"},
		{"IPv4 broadcast", "255.255.255.255"},

		// 4-in-6 包装。**这三条钉的是 checkIP 这一层**：拿掉它内部那一步
		// Unmap()，Azure 与阿里云两条会绿 —— 它们全局可路由或不在 RFC1918
		// 里，blockedPrefixes 是唯一拦得住它们的判定，而
		// netip.Prefix.Contains 不会自己拆包装。
		// 169.254 那条即使不 Unmap 也拦得住（IsLinkLocalUnicast 对 4-in-6
		// 仍为真），它在这里是对照，不构成对 Unmap 的证据。
		// 拨号那一层由 client_internal_test.go 的同名三条各自钉住。
		{"IPv4-mapped IPv6 Azure instance metadata", "::ffff:168.63.129.16"},
		{"IPv4-mapped IPv6 Alibaba metadata endpoint", "::ffff:100.100.100.200"},
		{"IPv4-mapped IPv6 GCP metadata endpoint, blocked either way", "::ffff:169.254.169.254"},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if err := checkIP(netip.MustParseAddr(c.ip)); !errors.Is(err, ErrBlockedDestination) {
				t.Errorf("checkIP(%s) = %v, want ErrBlockedDestination", c.ip, err)
			}
		})
	}
}

// 带 zone 的地址必须由 checkIP 自己拒绝，不靠调用方先剥。
//
// AWS 的 IPv6 元数据地址是 ULA，各项 Is* 判定都放行它，**只有网段比对
// 拦得住** —— 而 netip.Prefix.Contains 对带 zone 的地址一律返回 false，
// 落空就是放行。这条用例直接打 checkIP：拿掉它内部那一步 WithZone("")
// 就红。此前这半边完全依赖 guardedDial 先剥 zone，而解析器返回的地址
// 本来就不带 zone，那一步没有任何用例能证伪（安全规范补充版 §26）。
func TestCheckIPRefusesAZonedMetadataAddressOnItsOwn(t *testing.T) {
	zoned := netip.MustParseAddr("fd00:ec2::254%eth0")
	if err := checkIP(zoned); !errors.Is(err, ErrBlockedDestination) {
		t.Errorf("checkIP(%s) = %v, want ErrBlockedDestination", zoned, err)
	}
}

// 剥 zone 是为了让网段比对成立，不是把带 zone 的地址一律拒掉：
// 同一个 ULA 网段里的邻居地址带上 zone 仍然必须放行，否则这条守卫会
// 从"拦元数据端点"滑成"拦一整类地址"，放行表那一半就白写了。
func TestCheckIPStillAllowsAZonedAddressThatIsNotBlocked(t *testing.T) {
	zoned := netip.MustParseAddr("fd00:ec2::255%eth0")
	if err := checkIP(zoned); err != nil {
		t.Errorf("checkIP(%s) = %v, want nil", zoned, err)
	}
}

// 认不出形态时必须拒绝 —— 这一层的失败方向是关，不是开。
func TestCheckIPRefusesTheZeroAddress(t *testing.T) {
	if err := checkIP(netip.Addr{}); !errors.Is(err, ErrBlockedDestination) {
		t.Errorf("checkIP(zero Addr) = %v, want ErrBlockedDestination", err)
	}
}

// 拒绝原因不得带上地址或网段：它会冒泡到调用方并可能进日志，
// 内网地址属于敏感信息（安全规范 §19、§22）。
func TestBlockedDestinationErrorCarriesNoAddress(t *testing.T) {
	err := checkIP(netip.MustParseAddr("169.254.169.254"))
	if err == nil {
		t.Fatal("checkIP(169.254.169.254) = nil, want an error")
	}
	for _, leak := range []string{"169.254", "100.64", "168.63", "fd00", "fe80"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error text %q contains %q, want no address material", err.Error(), leak)
		}
	}
}
