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

	"k8s.io/client-go/kubernetes"

	"github.com/imkerbos/Distill/internal/buildinfo"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/kubeclient"
	applog "github.com/imkerbos/Distill/internal/log"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
	"github.com/imkerbos/Distill/internal/secrets/gcpsecrets"
	"github.com/imkerbos/Distill/internal/settings"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// defaultRunTimeout 是一次采集运行的默认上限。
//
// 有上限而不是等到天荒地老：采集是一串出站调用，而一次永远不返回的采集
// 是一次没人能取消的采集（规范 §30「无限等待」、§24）。上限到了之后
// 已经采到的部分照样落库，只是带着 TIMEOUT 的失败记录 —— 见 collectOnce。
const defaultRunTimeout = 10 * time.Minute

func main() {
	configPath := flag.String("config", "configs/demo.yaml", "path to the config file")
	clusterID := flag.String("cluster", "", "id of the registered cluster to collect")
	timeout := flag.Duration("timeout", defaultRunTimeout, "upper bound on one collection run")
	flag.Parse()

	if err := run(*configPath, *clusterID, *timeout); err != nil {
		// 日志器可能尚未构造成功，此处直接写 stderr。
		_, _ = os.Stderr.WriteString("collection failed: " + err.Error() + "\n")
		os.Exit(1)
	}
}

// run 采集一个集群并落库，返回过程中的致命错误。
func run(configPath, clusterID string, timeout time.Duration) error {
	if clusterID == "" {
		return errors.New("-cluster is required: name the registered cluster to collect")
	}
	// 非正超时不是"不限时"，是一个写错了的配置。默认值只在没给 -timeout
	// 时生效，显式给一个 0 必须被拒绝而不是静默变成无限等待。
	if timeout <= 0 {
		return errors.New("-timeout must be positive: an unbounded collection is one nobody can cancel")
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

	client, err := newClusterClient(ctx, reg, target)
	if err != nil {
		return err
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

	result, err := collectOnce(ctx, clusterID, client, fleet, snapshotstore.New(db), logger)
	if err != nil {
		return err
	}

	logger.Info("collection stored",
		"cluster", clusterID,
		"runId", result.Observation.RunID,
		"status", string(result.Status),
		"failures", len(result.Failures),
		"warnings", len(result.Observation.Warnings))
	return nil
}

// newClusterClient 把集群登记里的 kubeconfig 引用变成一个受守卫的客户端。
//
// 凭据只在这个函数的栈上以字节存在：不落临时文件、不进日志、不进错误信息
// （规范 §19、§33）。kubeconfig 里带的是一个能对 apiserver 说话的身份，
// 比 Git 那把 deploy key 重得多。**这是全平台唯一一处解析它的地方。**
func newClusterClient(ctx context.Context, reg registry.Store, target registry.Cluster) (kubernetes.Interface, error) {
	ref := target.KubeconfigRef
	if ref == "" {
		return nil, fmt.Errorf("cluster %q has no kubeconfig reference registered", target.ID)
	}
	// 引用来自数据库里操作者填的值，先过短名校验再交给解析器：
	// 短名会被拼进后端的资源路径，能表达的字符越多，能表达的越权路径越多。
	if err := secrets.ValidateRef(ref); err != nil {
		return nil, fmt.Errorf("cluster %q has an invalid kubeconfig reference: %w", target.ID, err)
	}

	// 设置只读一次：这是一个跑完就退出的进程，不存在"改了设置却要等重启"
	// 的问题 —— 那条禁止启动快照的规则针对的是常驻服务。
	setting, err := settings.New(reg).Current(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the platform setting: %w", err)
	}
	resolver, err := newSecretResolver(ctx, setting)
	if err != nil {
		return nil, err
	}
	if resolver == nil {
		// 后端是 NONE：操作者明确选了"不解析凭据"，于是也就没有采集。
		// 静默跳过会让这次运行看起来只是"什么都没采到"。
		return nil, errors.New("the secrets backend is NONE: the collector cannot resolve a kubeconfig")
	}

	kubeconfig, err := resolver.Resolve(ctx, ref)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return nil, fmt.Errorf("the kubeconfig reference %q resolves to nothing", ref)
		}
		// 底层错误不外传：目录后端的错误里带文件系统路径，Secret Manager
		// 的错误里带项目与资源名（规范 §22）。引用本身不是机密，说得出
		// 是哪一个引用就够操作者定位了。
		return nil, fmt.Errorf("cannot resolve the kubeconfig reference %q", ref)
	}
	// 解析出来的字节用完即清。清的是这一份原始副本 —— 客户端内部仍然
	// 持有它解出来的凭据，那是它能工作的前提；这里能保证的是这份可以被
	// 顺手写进日志或临时文件的字节切片活得尽可能短。
	defer clear(kubeconfig)

	client, err := kubeclient.New(kubeconfig)
	if err != nil {
		// 同样不外传：clientcmd 的解析错误会把 kubeconfig 的片段与
		// apiserver 地址一起带出来，而这个进程的输出终点是集群日志。
		return nil, fmt.Errorf("cannot build a Kubernetes client from the kubeconfig behind %q", ref)
	}
	return client, nil
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
