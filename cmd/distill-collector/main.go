// Command distill-collector 采集单个 Kubernetes 集群的资产快照并落库。
//
// 与 distill-api 分开的二进制，不是为了部署方便：CLAUDE.md 把"平台主服务
// 持有日常 Kubernetes 策略写权限"定为设计错误，而进程隔离是唯一能让这件事
// 成为**部署事实**的做法 —— kubeconfig 只在这个进程里被解析成凭据，
// 主服务存引用、显示引用，但没有任何一条把它变成凭据的路径
// （绊线见 internal/registry 的 kubecred_callsite_test.go，那里也写了
// 它拦不住什么）。
//
// 本轮触发方式是手动：跑一次、采一个集群、落一次库，然后退出。
// 没有调度器、没有常驻循环（spec §3）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/imkerbos/Distill/internal/buildinfo"
	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/kubeclient"
	applog "github.com/imkerbos/Distill/internal/log"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
	"github.com/imkerbos/Distill/internal/secrets/gcpsecrets"
	"github.com/imkerbos/Distill/internal/settings"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// defaultRunTimeout 是一次采集运行的默认上限。
//
// 有上限而不是等到天荒地老：采集是一串出站调用，而一次永远不返回的采集
// 是一次没人能取消的采集（规范 §30「无限等待」、§24）。上限到了之后
// 已经采到的部分照样落库，只是带着 TIMEOUT 的失败记录 —— 见 collectOnce。
const defaultRunTimeout = 10 * time.Minute

// defaultFlowWindow 是一次摄入默认回看的时间长度。
//
// 一小时与 spec §7 的量级估算同一个尺度：500 Pod、平均 8 个对端的集群，
// 一小时窗口约 4000 条连接。
const defaultFlowWindow = time.Hour

func main() {
	configPath := flag.String("config", "configs/demo.yaml", "path to the config file")
	clusterID := flag.String("cluster", "", "id of the registered cluster to collect")
	timeout := flag.Duration("timeout", defaultRunTimeout, "upper bound on one collection run")
	relay := flag.String("flow-relay", "", "host:port of the Hubble relay; empty means no flow ingestion")
	relayCred := flag.String("flow-relay-ca", "", "short-name reference to the relay TLS CA; empty means a plaintext connection")
	flowWindow := flag.Duration("flow-window", defaultFlowWindow, "how far back one flow ingestion looks")
	flag.Parse()

	ingest := ingestOptions{
		relayAddress:  *relay,
		credentialRef: *relayCred,
		window:        *flowWindow,
	}
	if err := run(*configPath, *clusterID, *timeout, ingest); err != nil {
		// 日志器可能尚未构造成功，此处直接写 stderr。
		//
		// 措辞不说"采集失败"：这一行同样会承载一次摄入失败，而一次采成功、
		// 摄入挂掉的运行若报成"采集失败"，操作者会去查一个完全正常的 RBAC。
		_, _ = os.Stderr.WriteString("collector run failed: " + err.Error() + "\n")
		os.Exit(1)
	}
}

// 推送式接入是另一个二进制（cmd/distill-agent）。
//
// 拆开而不是靠一个 -mode 开关（原先是那样的）：这个二进制直连平台状态库，
// 而推送式那个会被装进别人的集群 —— 只要它们还是同一个可执行文件，
// 「装进去的那个不带状态库访问路径」就只能靠 review 保证
// （design doc 2026-08-18 §1.2）。

// run 采集一个集群并落库，返回过程中的致命错误。
//
// ingest 为零值时只采资产。摄入与采集在这里是两件事，各自落自己的运行
// 记录，失败也不互相冒充 —— 见 collectAndIngest。
func run(configPath, clusterID string, timeout time.Duration, ingest ingestOptions) error {
	if clusterID == "" {
		return errors.New("-cluster is required: name the registered cluster to collect")
	}
	// 非正超时不是"不限时"，是一个写错了的配置。默认值只在没给 -timeout
	// 时生效，显式给一个 0 必须被拒绝而不是静默变成无限等待。
	if timeout <= 0 {
		return errors.New("-timeout must be positive: an unbounded collection is one nobody can cancel")
	}
	// 摄入选项与超时一样，在连库之前就校验：一份写错的编排文件应当让这次
	// 运行起不来，而不是先采完资产再在最后一步失败。
	if err := ingest.validate(); err != nil {
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger, err := applog.New(cfg.Log.Level, os.Stdout)
	if err != nil {
		return err
	}
	logger.Info("starting", "version", buildinfo.Version(), "cluster", clusterID)

	// 信号与超时叠在同一个 ctx 上：Ctrl-C 与超时对采集是同一件事 ——
	// "别再往集群要数据了"。两者之后已采到的部分仍然落库（collectOnce）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	db, err := mysqlregistry.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// **不跑迁移。** schema 由 distill-api 在启动时推进；采集器是一个可以
	// 被并发拉起多次的一次性进程，让它也迁移就是让两个进程同时改同一套
	// 表。落后于 schema 时这里会以一个具体的 SQL 错误失败，那比一次并发
	// 迁移留下的半套表更容易查。
	reg := mysqlregistry.New(db)

	target, ok, err := reg.Cluster(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("read the cluster registration: %w", err)
	}
	if !ok {
		return fmt.Errorf("cluster %q is not registered", clusterID)
	}

	// startedAt 在这里取，而不是等到 collectOnce：从这一行往下的每一次
	// 失败都还没读到集群，但都必须在历史里留下痕迹。
	startedAt := time.Now()
	store := snapshotstore.New(db)

	clients, probe, reason, err := newClusterClient(ctx, reg, target)
	if err != nil {
		// 凭据解析不出来、或客户端构造不出来，都是"这一轮根本没开始"。
		// 不落这一行，界面会把一次配置错误显示成"这个集群还没有被采集过"，
		// 于是操作者去等一次永远不会成功的采集。
		recordAbortedRun(ctx, store, clusterID, startedAt, reason, logger)
		return err
	}

	// 探测结论落库。**探测没查成时写 UNKNOWN，不是不写** ——不写会让上一次
	// 的结论留在那里，而那一次可能是在集群装 Cilium 之前做的
	// （design doc 2026-08-25 §2.3）。
	var planes registry.PolicyPlanes
	switch {
	case !probe.Checked:
		planes = registry.PlanesUnknown
	case probe.Present:
		planes = registry.PlanesPresent
	default:
		planes = registry.PlanesNone
	}
	if err := reg.SetOtherPlanes(ctx, clusterID, planes); err != nil {
		// 落不下去不中断采集：资产本身是有价值的，而判定会因为读到的仍是
		// 旧结论而**偏保守或偏乐观** —— 因此必须留下一条日志，不能静默。
		logger.Warn("could not record the policy-plane probe; the stored verdict may be stale",
			"cluster", clusterID, "planes", planes)
	} else if planes != registry.PlanesNone {
		logger.Info("this cluster carries policy planes the platform does not evaluate; "+
			"verdicts for it are DEGRADED",
			"cluster", clusterID, "planes", planes, "detail", probe.Detail)
	}

	// 判定依据取全 fleet 而不只是被采的这个集群：判断一个 Pod IP 是否落在
	// **别的**集群网段里，恰恰需要看得见别的集群（spec §3.2）。
	clusters, err := reg.Clusters(ctx)
	if err != nil {
		return fmt.Errorf("read the fleet registration: %w", err)
	}
	fleet, unusable := fleetRegistry(clusters)
	for _, id := range unusable {
		// 不中断采集：网段登记坏掉的后果是那个集群的 IP 判定退化成
		// UNKNOWN，而这正是 internal/cluster 为登记缺口准备的答案。
		// 中断会让一处填错的登记表现成"整个 fleet 采不了"。
		logger.Warn("a cluster has an unusable CIDR registration; IP scope will degrade to UNKNOWN", "cluster", id)
	}

	// 摄入器在采集之前装配：装配失败（地址写错、凭据后端是 NONE）是一次
	// 配置错误，让它在读集群之前就把这一轮拦下来，比采完资产再失败清楚。
	var src *flowSource
	if ingest.enabled() {
		src, err = newFlowSource(ctx, reg, ingest)
		if err != nil {
			return err
		}
	}
	// 窗口取到"现在"为止的一段：摄入是跟着这次运行走的，不是补历史。
	now := time.Now()
	window := flow.Window{From: now.Add(-ingest.window), To: now}

	// 对账器与读栈同源：它必须与 /flows 那一屏走同一条判定路径，否则
	// 一致率量的是另一个引擎（design doc 2026-08-25 §3.1）。
	return collectAndIngest(ctx, clusterID, clients.typed, clients.dynamic, fleet, store, src, window,
		collectstore.New(db, reg), reg, foreignPlanesOf(probe), logger)
}

// foreignPlanesOf 把探测结果翻成随快照落库的那一份覆盖范围。
//
// **探测没查成时一律不完整**：Checked 为 false 说明我们连"有没有第二平面"
// 都没问出来，那时任何精确到主体的说法都是编出来的。判定据此整片降级。
func foreignPlanesOf(probe kubeclient.PlaneProbe) collectrun.ForeignPlanes {
	if !probe.Checked {
		return collectrun.ForeignPlanes{}
	}
	scopes := make([]snapshot.ForeignScope, 0, len(probe.Scopes))
	for _, sc := range probe.Scopes {
		scopes = append(scopes, snapshot.ForeignScope{
			Namespace: sc.Namespace, MatchLabels: sc.MatchLabels,
		})
	}
	return collectrun.ForeignPlanes{Scopes: scopes, Complete: probe.ScopesComplete}
}

// newClusterClient 把集群登记里的 kubeconfig 引用变成一个受守卫的客户端。
//
// 凭据只在这个函数的栈上以字节存在：不落临时文件、不进日志、不进错误信息
// （规范 §19、§33）。kubeconfig 里带的是一个能对 apiserver 说话的身份，
// 比 Git 那把 deploy key 重得多。**这是全平台唯一一处解析它的地方。**
// 第二个返回值是失败时该记进 collection_run.error_reason 的封闭原因；
// 成功时为 snapshot.RunErrorNone。返回它而不是让调用方去猜：错误文本
// 刻意不带底层细节，从它反推原因只能靠字符串匹配。
func newClusterClient(
	ctx context.Context, reg registry.Store, target registry.Cluster,
) (clusterClients, kubeclient.PlaneProbe, snapshot.RunErrorReason, error) {
	ref := target.KubeconfigRef
	if ref == "" {
		return clusterClients{}, kubeclient.PlaneProbe{}, snapshot.RunErrorCredentialUnavailable,
			fmt.Errorf("cluster %q has no kubeconfig reference registered", target.ID)
	}
	// 引用来自数据库里操作者填的值，先过短名校验再交给解析器：
	// 短名会被拼进后端的资源路径，能表达的字符越多，能表达的越权路径越多。
	if err := secrets.ValidateRef(ref); err != nil {
		return clusterClients{}, kubeclient.PlaneProbe{}, snapshot.RunErrorCredentialUnavailable,
			fmt.Errorf("cluster %q has an invalid kubeconfig reference: %w", target.ID, err)
	}

	// 设置只读一次：这是一个跑完就退出的进程，不存在"改了设置却要等重启"
	// 的问题 —— 那条禁止启动快照的规则针对的是常驻服务。
	setting, err := settings.New(reg).Current(ctx)
	if err != nil {
		return clusterClients{}, kubeclient.PlaneProbe{}, snapshot.RunErrorCredentialUnavailable,
			fmt.Errorf("read the platform setting: %w", err)
	}
	resolver, err := newSecretResolver(ctx, setting)
	if err != nil {
		return clusterClients{}, kubeclient.PlaneProbe{}, snapshot.RunErrorCredentialUnavailable, err
	}
	if resolver == nil {
		// 后端是 NONE：操作者明确选了"不解析凭据"，于是也就没有采集。
		// 静默跳过会让这次运行看起来只是"什么都没采到"。
		return clusterClients{}, kubeclient.PlaneProbe{}, snapshot.RunErrorCredentialUnavailable,
			errors.New("the secrets backend is NONE: the collector cannot resolve a kubeconfig")
	}

	kubeconfig, err := resolver.Resolve(ctx, ref)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return clusterClients{}, kubeclient.PlaneProbe{}, snapshot.RunErrorCredentialUnavailable,
				fmt.Errorf("the kubeconfig reference %q resolves to nothing", ref)
		}
		// 底层错误不外传：目录后端的错误里带文件系统路径，Secret Manager
		// 的错误里带项目与资源名（规范 §22）。引用本身不是机密，说得出
		// 是哪一个引用就够操作者定位了。
		return clusterClients{}, kubeclient.PlaneProbe{}, snapshot.RunErrorCredentialUnavailable,
			fmt.Errorf("cannot resolve the kubeconfig reference %q", ref)
	}
	// 解析出来的字节用完即清。清的是这一份原始副本 —— 客户端内部仍然
	// 持有它解出来的凭据，那是它能工作的前提；这里能保证的是这份可以被
	// 顺手写进日志或临时文件的字节切片活得尽可能短。
	defer clear(kubeconfig)

	client, err := kubeclient.New(kubeconfig)
	if err != nil {
		// 同样不外传：clientcmd 的解析错误会把 kubeconfig 的片段与
		// apiserver 地址一起带出来，而这个进程的输出终点是集群日志。
		// 凭据本身拿到了，构造不出客户端 —— 包括 apiserver 地址被出站守卫
		// 拒绝。与凭据不可用分开：两者的处置一个是改凭据，一个是改地址。
		return clusterClients{}, kubeclient.PlaneProbe{}, snapshot.RunErrorClientUnavailable,
			fmt.Errorf("cannot build a Kubernetes client from the kubeconfig behind %q", ref)
	}
	// 平面探测与客户端构造放在一起：**它要用的正是这份马上就要被清掉的
	// kubeconfig 字节**，挪到别处就得让凭据多活一段，或者多解析一次。
	// 探测失败不影响采集 —— 它的失败方向是把结论标成"没查成"，那正是
	// 下游要的（design doc 2026-08-25 §2.3）。
	probe, err := kubeclient.ProbePlanes(ctx, kubeconfig)
	if err != nil {
		probe = kubeclient.PlaneProbe{}
	}

	// 动态客户端同样在这里建，理由与探测一致：它要的也是这份就要被清掉的
	// kubeconfig。构造失败**不中止采集** —— 丢的只是 ANP 这一类，而那一类
	// 会自己以一条采集失败的形式记进本次运行，比整轮放弃更接近事实。
	dyn, err := kubeclient.NewDynamic(kubeconfig)
	if err != nil {
		dyn = nil
	}
	return clusterClients{typed: client, dynamic: dyn}, probe, snapshot.RunErrorNone, nil
}

// clusterClients 是一个目标集群的两个客户端。
//
// 装进一个结构体而不是多返回一个值：这个函数已经返回四样东西，
// 而第五个裸接口值在调用点上与第一个长得一模一样。
type clusterClients struct {
	// typed 读内建资源。
	typed kubernetes.Interface
	// dynamic 读 CRD（当前只有 ANP 一族）。为 nil 表示这一类采不了。
	dynamic dynamic.Interface
}

// newSecretResolver 按设置选出的后端装配凭据解析器。
//
// 与 cmd/distill-api 的同名函数同形并且刻意重复：把它抽成共享包会让**解析
// 凭据的能力**成为一个两个二进制都 import 的东西，而 internal/registry 的
// 绊线正是按"引用与解析能力不得共存"来判的。这份重复的代价（新增一个
// 后端要改两处）小于把能力搬到主服务边上的代价，而漏改的失败方向是
// 采集器起不来，不是主服务多拿到一份凭据。
//
// 每个分支都显式写出返回值，不用直接转发：构造失败时后端返回的是一个
// nil 具体指针，装进接口就成了非 nil 的接口值，而调用方正是用
// `resolver == nil` 判断"没有解析器"的。
func newSecretResolver(ctx context.Context, s registry.PlatformSetting) (secrets.Resolver, error) {
	switch s.SecretsBackend {
	case registry.SecretsBackendDir:
		return secrets.NewDirResolver(s.SecretsDir), nil
	case registry.SecretsBackendSecretManager:
		r, err := gcpsecrets.NewResolver(ctx, s.SecretsProject, s.SecretsPrefix)
		if err != nil {
			return nil, err
		}
		return r, nil
	case registry.SecretsBackendNone:
		return nil, nil
	default:
		// SecretsBackend 是封闭枚举；走到这里说明加了取值却没加装配分支。
		return nil, fmt.Errorf("unknown secrets backend %q", s.SecretsBackend)
	}
}
