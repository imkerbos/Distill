// Package kubeclient 构造采集用的 Kubernetes 客户端。
//
// 全平台唯一构造 client-go 客户端的地方。集中在这里是为了让"拨号必须
// 先过地址守卫"成为一条可以检查的性质：apiserver 地址来自注册表，
// 由操作者填写，是一个外部输入。
package kubeclient

import (
	"context"
	"fmt"
	"net"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// dialTimeout 是单次 TCP 拨号的上限。
	dialTimeout = 10 * time.Second
	// requestTimeout 是单次 API 请求的上限。
	//
	// 有上限而非无限等待：采集器会被定时拉起，一次卡死的运行会让后续
	// 每一次都堆在它后面，而"没有新数据"在界面上与"集群没有变化"
	// 无法区分。
	requestTimeout = 60 * time.Second
	// userAgent 让集群侧的审计日志能认出这些请求来自本平台。
	userAgent = "distill-collector"
)

// New 用 kubeconfig 构造客户端，并把出站拨号钉在地址守卫后面。
func New(kubeconfig []byte) (kubernetes.Interface, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeclient: parse kubeconfig: %w", err)
	}
	return newFromConfig(cfg)
}

// newFromConfig 在给定的 rest.Config 上装好守卫与超时。
func newFromConfig(cfg *rest.Config) (kubernetes.Interface, error) {
	cfg.UserAgent = userAgent
	cfg.Timeout = requestTimeout
	cfg.Dial = guardedDial

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubeclient: build client: %w", err)
	}
	return client, nil
}

// GuardedDialContext 是 guardedDial 的导出入口，供集群内的其他出站路径复用。
//
// 存在的理由是**不让这份网段策略出现第二份**。集群内还有别的东西要拨号 ——
// 第一个是 internal/hubble 直连 Hubble relay 的 gRPC。它需要的判定与
// apiserver 完全一致：relay 和 apiserver 一样在集群内，照搬 internal/gitssh
// 那套"拒绝一切私有地址"会把正常情况整个挡掉（见 destination.go 的注释）。
// 而在第二个包里重抄一遍 blockedPrefixes，两份必然漂移，且漂移的那一天没有
// 任何症状 —— 守卫仍然存在、仍然被测到，只是守的不是同一件事。
//
// 签名与 net.Dialer.DialContext 一致，因此 rest.Config.Dial 与
// grpc.WithContextDialer 都能直接接上。
func GuardedDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return guardedDial(ctx, network, address)
}

// guardedDial 解析一次主机名，逐个判定候选地址，只拨通过判定的那一个。
//
// 自己解析而非先判定主机名再交给传输层解析：两次解析之间就是 DNS
// rebinding 窗口（安全规范补充版 §24）。按解析结果判定、再直接拨这个 IP，
// 这个窗口就不存在。
//
// 传给 tls 的 ServerName 仍取自请求 URL 的主机名，不受这里替换拨号地址
// 影响，证书校验照常。
func guardedDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrBlockedDestination
	}

	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("kubeclient: resolve apiserver host: %w", err)
	}

	dialer := &net.Dialer{Timeout: dialTimeout}
	var lastErr error
	for _, ip := range ips {
		// 去掉 zone 与 4-in-6 包装后再判定，否则 ::ffff:169.254.169.254
		// 这类写法会绕过按 IPv4 写的网段比对。
		ip = ip.Unmap().WithZone("")
		if err := checkIP(ip); err != nil {
			lastErr = err
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}

	if lastErr == nil {
		lastErr = ErrBlockedDestination
	}
	return nil, lastErr
}
