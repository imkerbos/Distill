package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// 租约参数。
//
// 取值按 client-go 的常规区间，不追求"快"：抢得快的代价是续期请求更密，
// 而这个 Lease 一秒钟都不需要精确 —— 资产采集的间隔是三十分钟，晚十几秒
// 换手对任何人都没有区别。
const (
	leaseDuration = 30 * time.Second
	renewDeadline = 20 * time.Second
	retryPeriod   = 5 * time.Second
)

// leaderGate 报告本进程此刻是不是 leader。
//
// 用原子布尔而不是 channel：调用方是每一轮现问一次，而 leadership 会在
// 进程生命周期内变化 —— 一个启动时抄下来的答案会让刚接手的 Pod 永远不采
// 资产、或让刚失去 leadership 的 Pod 继续采（两份资产会撞 observed_at 主键）。
//
// changed 是一个独立的**通知**：状态可能变了，快去 isLeader 现问一次。
// 它存在的唯一理由是让资产循环对「刚当选」立刻反应，而不是空等一整个
// assetsEvery（生产 30m）。缓冲 1 + 非阻塞发送：信号只是「去看一眼」，
// 多个挤在一起合并成一个就够，绝不阻塞选举回调。
type leaderGate struct {
	held    atomic.Bool
	changed chan struct{}
}

// newLeaderGate 建一个带通知通道的 gate。
func newLeaderGate() *leaderGate {
	return &leaderGate{changed: make(chan struct{}, 1)}
}

func (g *leaderGate) isLeader(context.Context) bool { return g.held.Load() }

// notify 非阻塞地发一个「状态可能变了」的信号。
//
// 缓冲已满（已有一个待处理信号）时直接丢弃：两个「去看一眼」合并成一个，
// 消费方看到的仍是最新的 held。绝不阻塞 —— 这跑在选举回调里。
func (g *leaderGate) notify() {
	if g.changed == nil {
		return
	}
	select {
	case g.changed <- struct{}{}:
	default:
	}
}

// runLeaderElection 在后台参与选举，并把结果反映到 gate 上。
//
// **输了选举与读不了 Lease 必须分得开。** 前者是常态（别的 Pod 是 leader），
// 后者是权限配错。塌成一个的话症状相同：这个集群永远没有资产采集，而没有
// 任何东西说得出为什么（design doc 2026-08-19-unified-agent §4）。
//
// ReleaseOnCancel：SIGTERM 时主动让出租约，下一个 leader 不必等整个租约
// 超时 —— 那段时间没有任何 Pod 在采资产。
func runLeaderElection(
	ctx context.Context, client kubernetes.Interface,
	namespace, name, identity string, gate *leaderGate, logger *slog.Logger,
) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: name, Namespace: namespace},
		Client:     client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {
				gate.held.Store(true)
				// 先置位再通知：资产循环收到信号后会 isLeader 现问，那时
				// held 必须已经是 true，否则这次当选的立即采集会被漏掉。
				gate.notify()
				logger.Info("this pod now collects the cluster's assets", "identity", identity)
			},
			OnStoppedLeading: func() {
				// 立刻放下：还握着的话，下一轮会与新 leader 同时采一份资产，
				// 而两份会撞 observed_at 主键。
				gate.held.Store(false)
				gate.notify()
				logger.Info("this pod no longer collects the cluster's assets", "identity", identity)
			},
			OnNewLeader: func(who string) {
				if who == identity {
					return
				}
				// **输选举是 Info，不是错误。** 每个节点一个 Pod，输的是多数，
				// 报成错误会让日志里全是它，而真正该看的那条（Lease 读不了）
				// 会淹在里面。
				logger.Info("another pod collects the cluster's assets", "leader", who)
			},
		},
	})
}

// saNamespaceFile 是 kubelet 投进来的那份 ServiceAccount 里的 namespace。
const saNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// resolveNamespace 决定那个 Lease 落在哪个 namespace。
//
// 顺序是刻意的：显式参数 > ServiceAccount 挂载。
//
// **不从环境变量兜底**（比如 POD_NAMESPACE）：那要求部署清单记得配 downward
// API，而忘了配的症状是 Lease 落进 "default" —— 一个跑在别的 namespace 的
// agent 于是与它无关的进程抢同一把锁，两个集群的资产采集互相踩。
// SA 挂载是 kubelet 无条件投进来的，没有"忘了配"这种形态。
func resolveNamespace(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	raw, err := os.ReadFile(saNamespaceFile)
	if err != nil {
		return "", errors.New(
			"cannot tell which namespace this pod runs in: pass -lease-namespace explicitly")
	}
	ns := strings.TrimSpace(string(raw))
	if ns == "" {
		return "", errors.New("this pod's ServiceAccount namespace file is empty")
	}
	return ns, nil
}
