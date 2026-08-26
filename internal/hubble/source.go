// Package hubble 直连集群内的 Hubble relay，把真实流量读成 flow.IngestResult。
//
// 这是平台第一个**真实**的流量来源 —— 在它之前，判定引擎看到的每一条连接
// 都来自 internal/fixture 的合成数据。
//
// 本包是边缘层：它做 I/O、依赖 gRPC。纯逻辑（连接的类型、完整度怎么算）
// 全在 internal/flow，本包只负责把 Hubble 的消息翻译过去，且**只往"不知道"
// 的方向翻**：Hubble 没报的判定不得变成放行，问不出来的丢弃计数不得变成
// "一条没漏"，标签缺失的对端不得凭空得到一个身份（spec §2、§5）。
//
// 走 gRPC 而不是 shell 出去调 hubble CLI：test/conformance 那样做是测试
// 便利，一条生产路径不能依赖某台机器上装没装一个外部二进制。
package hubble

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/cilium/cilium/api/v1/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/kubeclient"
	"github.com/imkerbos/Distill/internal/secrets"
)

const (
	// defaultTimeout 是单次摄入的上限。
	//
	// 有上限而非无限等待：摄入器会被定时拉起，一次卡死的运行会让后续每一次
	// 都堆在它后面（与 kubeclient.requestTimeout 同一条理由）。上限到了就
	// 报错 —— **绝不返回一个空窗口**，因为"我们放弃了"与"这段时间没有流量"
	// 在下游是天差地别的两件事。
	defaultTimeout = 60 * time.Second

	// maxFlowsPerIngest 是单次摄入读取的 flow 上限（安全规范 §24）。
	//
	// relay 那一端的流量是别人的集群产生的，不是我们能约束的输入；不设上限
	// 就等于把进程内存交给它决定。达到上限时不是静默截断：覆盖窗口据此缩到
	// 我们真正看见的最后一刻，完整度落 DEGRADED —— 而那一刻没被量到时退回
	// UNKNOWN，理由见 covered。
	maxFlowsPerIngest = 100_000
)

// ErrRelayUnavailable 表示连不上 relay。
//
// 刻意不带地址、主机名或任何传输层细节：这个错误会冒泡并进日志，而 relay
// 的地址属于内网拓扑信息（安全规范 §19、§22）。gRPC 与 DNS 的原始错误文本
// 里都带地址，因此它们一律不外传，只留下这一句和一个 gRPC 状态码。
var ErrRelayUnavailable = errors.New("hubble: relay is not reachable")

// ErrRelayStream = flow 流在中途失败。同样不带传输层细节。
var ErrRelayStream = errors.New("hubble: relay flow stream failed")

// ErrInvalidCredential 表示 relay 凭据取到了但解析不出证书。
//
// 独立成一个错误而不是回退明文：一份坏掉的 CA 如果导致降级到明文，
// 症状是"它还在工作"，而实际上传输已经不再被验证。
var ErrInvalidCredential = errors.New("hubble: relay credential contains no usable certificate")

// dialFunc 是一次出站拨号，签名与 net.Dialer.DialContext 一致。
//
// 做成字段是为了让测试能换成一个进程内的假 relay。生产路径上它只有一个
// 取值 —— kubeclient.GuardedDialContext，见 New。
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Config 是摄入器的构造参数。
type Config struct {
	// Address 是 Hubble relay 的 host:port。
	Address string
	// CredentialRef 是 relay TLS 的 CA 短名引用（与 apiserver 同一套凭据
	// 机制，spec §5）。留空表示明文连接 —— 只在 relay 位于集群内或经由
	// 端口转发到本机时才成立。
	CredentialRef string
	// Secrets 解析 CredentialRef。CredentialRef 非空时必填。
	Secrets secrets.Resolver
	// Timeout 是单次摄入的上限，非正数时用默认值。
	Timeout time.Duration
}

// Source 从 Hubble relay 摄入流量，实现 flow.Source。
type Source struct {
	address       string
	host          string
	credentialRef string
	secrets       secrets.Resolver
	timeout       time.Duration
	dial          dialFunc
}

// 编译期钉住契约：本包的存在意义就是成为 flow.Source 的第一个真实实现。
var _ flow.Source = (*Source)(nil)

// New 构造一个直连 Hubble relay 的摄入器。
//
// **出站拨号在这里被钉到 kubeclient.GuardedDialContext 上。** 复用 apiserver
// 那一套判定而不是另写一份：relay 与 apiserver 同样在集群内，internal/gitssh
// 那种"拒绝一切私有地址"的策略会把正常情况整个挡掉；而在本包重抄一份网段表，
// 两份必然漂移（见 kubeclient/destination.go 的注释）。
func New(cfg Config) (*Source, error) {
	if cfg.Address == "" {
		return nil, errors.New("hubble: empty relay address")
	}
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil || host == "" {
		// 不回显 cfg.Address：它是内网地址。
		return nil, errors.New("hubble: relay address must be host:port")
	}
	if cfg.CredentialRef != "" {
		if err := secrets.ValidateRef(cfg.CredentialRef); err != nil {
			return nil, fmt.Errorf("hubble: relay credential ref: %w", err)
		}
		if cfg.Secrets == nil {
			return nil, errors.New("hubble: credential ref given without a resolver")
		}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Source{
		address:       cfg.Address,
		host:          host,
		credentialRef: cfg.CredentialRef,
		secrets:       cfg.Secrets,
		timeout:       timeout,
		dial:          kubeclient.GuardedDialContext,
	}, nil
}

// Ingest 读取一个时间窗内 relay 报告的流量。
//
// 失败一律返回 error 而不是一份空结果：一次超时、一次连不上 relay 若表现成
// "这个窗口没有连接"，下游会把它当作"这些规则没有流量、可以收紧"，进而推荐
// 一份切断现有连接的策略（spec §2）。这是本平台唯一的单向错误方向。
func (s *Source) Ingest(ctx context.Context, clusterID string, window flow.Window) (flow.IngestResult, error) {
	// clusterID 在本包不参与取数（一个 relay 只对着一个集群），但空值必须
	// 当场拒绝：一批说不出自己属于哪个集群的连接落到事实层会 join 到别的
	// 集群的 Pod 上，且不报错（CLAUDE.md §4）。
	if clusterID == "" {
		return flow.IngestResult{}, errors.New("hubble: empty cluster id")
	}
	if !window.Valid() {
		return flow.IngestResult{}, errors.New("hubble: invalid ingest window")
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	conn, err := s.connect(ctx)
	if err != nil {
		return flow.IngestResult{}, err
	}
	defer func() { _ = conn.Close() }()

	stream, err := observer.NewObserverClient(conn).GetFlows(ctx, &observer.GetFlowsRequest{
		Since: timestamppb.New(window.From),
		Until: timestamppb.New(window.To),
	})
	if err != nil {
		return flow.IngestResult{}, s.streamError(ctx, err)
	}

	acc := newAccumulator()
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return flow.IngestResult{}, s.streamError(ctx, err)
		}
		if full := acc.observe(resp); full {
			break
		}
	}
	return acc.result(window)
}

// streamError 把 gRPC 的失败翻译成一个不带地址的错误。
//
// 超时单独成一条并保留 context.DeadlineExceeded：调用方要能区分"relay 拒绝了
// 我们"与"我们等不下去了"，而两者都必须区别于"这个窗口没有流量"。
// 其余情况只留下 gRPC 状态码 —— 它是排查线索，且不含地址；原始 error 文本
// 里带 `dial tcp <ip>:<port>` 这类内容，不得外传（安全规范 §19、§22）。
func (s *Source) streamError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("hubble: ingest gave up before the window was read: %w", ctxErr)
	}
	// **不能只看 ctx.Err()。** 我们自己给这次调用设了 deadline，gRPC 可能先
	// 带着 DeadlineExceeded 状态码返回，而客户端 context 还没被标记过期 ——
	// 两者之间有一段真实存在的窗口，机器一忙就会命中（本条判断补上之前，
	// 那条用例在全量并行跑时随机变红，单跑必过）。
	//
	// 落进通用分支的后果不只是测试红：调用方据此分不清"我们等不下去了"与
	// "relay 拒绝了我们"，而那正是这个函数存在的理由。
	if status.Code(err) == codes.DeadlineExceeded {
		return fmt.Errorf("hubble: ingest gave up before the window was read: %w",
			context.DeadlineExceeded)
	}
	return fmt.Errorf("%w (grpc code %s)", ErrRelayStream, status.Code(err))
}

// connect 先过地址守卫拨出这一条连接，再把它交给 gRPC。
//
// 自己先拨、再把既有连接交给 gRPC，而不是让 gRPC 拿着地址去拨：守卫的拒绝
// 才能原样返回给调用方。交给 gRPC 拨的话，拒绝会被裹进一个 Unavailable 状态、
// 错误文本里还会带上地址，errors.Is 也就认不出它了。
//
// target 用 passthrough scheme：主机名已经由守卫解析并判定过，让 gRPC 再解析
// 一次就是一个 DNS rebinding 窗口（安全规范补充版 §24）。
func (s *Source) connect(ctx context.Context) (*grpc.ClientConn, error) {
	creds, err := s.transportCredentials(ctx)
	if err != nil {
		return nil, err
	}

	raw, err := s.dial(ctx, "tcp", s.address)
	if err != nil {
		// 只放行守卫自己的拒绝：它刻意不带地址。DNS 失败与连接被拒的错误
		// 文本里带主机名或 IP，一律换成不带细节的 ErrRelayUnavailable。
		if errors.Is(err, kubeclient.ErrBlockedDestination) {
			return nil, fmt.Errorf("hubble: refusing to reach relay: %w", err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrRelayUnavailable, ctxErr)
		}
		return nil, ErrRelayUnavailable
	}

	conn, err := grpc.NewClient("passthrough:///"+s.address,
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(handOver(raw)),
	)
	if err != nil {
		_ = raw.Close()
		return nil, ErrRelayUnavailable
	}
	return conn, nil
}

// handOver 把一条已经过守卫的连接交给 gRPC，并拒绝任何重拨。
//
// 拒绝重拨是刻意的：第二次拨号就绕过了上面那次守卫判定。一次摄入是短命的
// 单次流，断了就该报错 —— 而"报错"正是这里要的失败方向。
func handOver(c net.Conn) func(context.Context, string) (net.Conn, error) {
	var mu sync.Mutex
	used := false
	return func(context.Context, string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if used {
			return nil, ErrRelayUnavailable
		}
		used = true
		return c, nil
	}
}

// transportCredentials 按凭据引用构造传输层凭据。
//
// 凭据取不到或解析不出证书时一律失败，**绝不回退明文**：那种回退的症状是
// "它还在工作"，而实际上已经没有任何东西在验证对端是不是 relay。
func (s *Source) transportCredentials(ctx context.Context) (credentials.TransportCredentials, error) {
	if s.credentialRef == "" {
		return insecure.NewCredentials(), nil
	}

	pem, err := s.secrets.Resolve(ctx, s.credentialRef)
	if err != nil {
		return nil, fmt.Errorf("hubble: resolve relay credential: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		// 不回显 pem：它是凭据内容，不进错误也不进日志（安全规范 §19）。
		return nil, ErrInvalidCredential
	}
	return credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: s.host,
		MinVersion: tls.VersionTLS12,
	}), nil
}
