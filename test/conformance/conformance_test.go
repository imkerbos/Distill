// Package conformance_test 拿一个真实运行 Cilium 的 kind 集群和求值引擎
// 对账：同一批策略、同一批真实流量，两个独立实现必须给出同一个结论。
//
// CLAUDE.md 要求 internal/replay 接近 100% 正确，而人工 review 保证不了
// 这件事——golden test 只测出作者自己想到的场景。这份 harness 反过来：
// 集群和 Hubble 是不知道我们怎么实现引擎的第三方，它们的判定才是能
// 兜底"我们理解错了语义"这类缺陷的独立信号。
//
// 环境不常驻：默认跳过，需要 DISTILL_CONFORMANCE_CONTEXT 显式指定
// kube context 才会运行，这与 internal/mysqlregistry 用
// DISTILL_TEST_MYSQL_DSN 跳过集成测试是同一惯例——见该包的
// db_test.go。集群搭建见 test/conformance/setup.sh、README.md。
package conformance_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/imkerbos/Distill/internal/replay"
)

// envKubeContext 指定要连接的 kube context。取值必须显式给出，绝不
// 落到"当前 context"上——这份 harness 会读一个真实集群的策略和流量，
// 误连生产集群是本平台整个存在理由的反面（同一约束见 tmp/probe/main.go）。
const envKubeContext = "DISTILL_CONFORMANCE_CONTEXT"

// envHubbleAddr 指定 Hubble relay 地址；未设置时用本地端口转发的默认值。
const envHubbleAddr = "DISTILL_CONFORMANCE_HUBBLE"

// defaultHubbleAddr 对应 setup.sh 起的 hubble-relay port-forward。
const defaultHubbleAddr = "localhost:4245"

// flowWindow 是收集 Hubble flow 的时长。夹具里的流量发生器每 2~5 秒
// 循环一轮，20 秒足够每条连接跑出多个样本，同时不让本地开发时的单次
// 运行拖太久。
const flowWindow = 20 * time.Second

// TestConformance 是唯一的入口：加载集群、收集一个窗口的 Hubble flow、
// 用引擎对同一批 flow 求值，再按声明式期望矩阵逐条对账。
//
// 未设置 DISTILL_CONFORMANCE_CONTEXT 时跳过——这让 `make check` 和
// `go test ./...` 在没有集群的机器上依然全绿，符合"框架只待在边缘层、
// 纯净层测试不依赖外部环境"的分层要求（这个包本身不是纯净层，但它
// 依赖的 internal/replay 是，所以它自己的跳过策略不能拖累那一层）。
func TestConformance(t *testing.T) {
	kubeContext := os.Getenv(envKubeContext)
	if kubeContext == "" {
		t.Skipf("%s not set; skipping conformance harness (see test/conformance/README.md)", envKubeContext)
	}
	hubbleAddr := os.Getenv(envHubbleAddr)
	if hubbleAddr == "" {
		hubbleAddr = defaultHubbleAddr
	}

	ctx := context.Background()
	namespaces, pods, policies := loadCluster(t, ctx, kubeContext)
	podIndex := indexPods(pods)

	t.Logf("loaded %d namespaces, %d pods, %d NetworkPolicies from context %q",
		len(namespaces), len(pods), len(policies), kubeContext)

	flows := collectFlows(t, hubbleAddr, flowWindow)
	t.Logf("collected %d TCP flows with resolved pod identities over %s", len(flows), flowWindow)

	evaluator := replay.NewEvaluator(kubeContext, policies, namespaces)

	for _, c := range expectedMatrix {
		t.Run(c.name, func(t *testing.T) {
			matched := flowsFor(flows, c)
			// 一个在期望矩阵里但在观测流量里完全没出现的用例必须响亮地失败：
			// 沉默在这里等价于"流量发生器停了，这一轮跑等于什么都没证明"，
			// 而不是"这条连接被拒绝所以没有流量"——DENY 用例在 matrix 里
			// 依然应该有 DROPPED flow，因为拒绝本身也是一条 flow。
			if len(matched) == 0 {
				t.Fatalf("no hubble flows observed for case %q (%s -> %s:%d); "+
					"traffic generator may have stopped, this run proves nothing for this case",
					c.name, c.sourceNamespace, c.destNamespace, c.port)
			}

			hubbleVerdict := effectiveVerdict(matched)
			if hubbleVerdict != c.verdict {
				t.Errorf("case %s: table says %s but cilium observed %s (%d flows)",
					c.name, c.verdict, hubbleVerdict, len(matched))
			}

			engineVerdict := evaluateCase(t, evaluator, kubeContext, podIndex, matched)
			if engineVerdict != c.verdict {
				t.Errorf("case %s: expected %s, engine said %s", c.name, c.verdict, engineVerdict)
			}
		})
	}
}

// expectedCase 是矩阵里的一行：一个连接形状和两个独立实现都必须落到的
// 判定。按 (源 namespace, 目的 namespace, 端口) 分组，而不是具体 Pod——
// Deployment 的副本数、Pod 名的哈希后缀都会变，namespace+端口才是这份
// 夹具真正想表达的"连接形状"。
type expectedCase struct {
	// name 出现在测试失败信息和 `go test -v` 输出里，必须一眼看出测的
	// 是什么语义，而不是一串 Pod IP。
	name            string
	sourceNamespace string
	destNamespace   string
	port            int32
	verdict         replay.Verdict
}

// expectedMatrix 是这份 harness 的规格说明本身：manifests/ 里的策略在
// 真实集群上应该产生的判定。改策略必须同步改这张表，改这张表也必须
// 说明白改的理由——两者不允许静默漂移（CLAUDE.md §7）。
var expectedMatrix = []expectedCase{
	{
		name: "namespace-selector-allow", sourceNamespace: "gateway", destNamespace: "payment",
		port: 8080, verdict: replay.VerdictAllow,
	},
	{
		name: "namespace-selector-deny", sourceNamespace: "batch", destNamespace: "payment",
		port: 8080, verdict: replay.VerdictDeny,
	},
	{
		name: "unlabelled-pod-deny", sourceNamespace: "legacy", destNamespace: "payment",
		port: 8080, verdict: replay.VerdictDeny,
	},
	{
		name: "named-port", sourceNamespace: "client", destNamespace: "probe-named",
		port: 8080, verdict: replay.VerdictAllow,
	},
	{
		name: "port-range-in-range", sourceNamespace: "client", destNamespace: "probe-range",
		port: 9090, verdict: replay.VerdictAllow,
	},
	{
		name: "port-range-out-of-range", sourceNamespace: "client", destNamespace: "probe-range",
		port: 9190, verdict: replay.VerdictDeny,
	},
	// 见 manifests/matrix.yaml 里 ipblock-except 上方的注释：这个单节点
	// kind 集群把所有业务 Pod 都放进了被 except 挖掉的那个 /24，所以这
	// 条连接的正确判定是 DENY，不是从策略名字直觉推出的 ALLOW。
	{
		name: "ipblock-except", sourceNamespace: "client", destNamespace: "probe-ipblock",
		port: 8080, verdict: replay.VerdictDeny,
	},
	{
		name: "egress-allow", sourceNamespace: "client-egress", destNamespace: "probe-named",
		port: 8080, verdict: replay.VerdictAllow,
	},
	{
		name: "egress-deny-no-ingress-policy", sourceNamespace: "client-egress", destNamespace: "probe-open",
		port: 8080, verdict: replay.VerdictDeny,
	},
}

// loadCluster 把真实集群读成求值引擎需要的纯类型：NamespaceRef、PodRef、
// 原生 networkingv1.NetworkPolicy。做法与 tmp/probe/main.go 一致——这个
// harness 就是那次探针的正式落地，转换逻辑不应该另起一套。
func loadCluster(
	t *testing.T, ctx context.Context, kubeContext string,
) ([]replay.NamespaceRef, []replay.PodRef, []networkingv1.NetworkPolicy) {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig for context %q: %v", kubeContext, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build kubernetes client: %v", err)
	}

	nsList, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	namespaces := make([]replay.NamespaceRef, 0, len(nsList.Items))
	for _, n := range nsList.Items {
		namespaces = append(namespaces, replay.NamespaceRef{
			ClusterID: kubeContext, Name: n.Name, Labels: n.Labels,
		})
	}

	podList, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	pods := make([]replay.PodRef, 0, len(podList.Items))
	for _, p := range podList.Items {
		pods = append(pods, podRefFrom(kubeContext, p))
	}

	npList, err := cs.NetworkingV1().NetworkPolicies("").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list network policies: %v", err)
	}

	return namespaces, pods, npList.Items
}

// podRefFrom 把 corev1.Pod 收窄成引擎需要的 PodRef，字段选取与
// tmp/probe/main.go 保持一致。
func podRefFrom(clusterID string, p corev1.Pod) replay.PodRef {
	var named []replay.NamedPort
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			named = append(named, replay.NamedPort{
				Name: port.Name, Port: port.ContainerPort,
				Protocol: replay.Protocol(port.Protocol),
			})
		}
	}
	return replay.PodRef{
		ClusterID: clusterID, Namespace: p.Namespace, Name: p.Name,
		IP: p.Status.PodIP, Labels: p.Labels,
		HostNetwork: p.Spec.HostNetwork, NamedPorts: named,
	}
}

// indexPods 建立 "namespace/name" -> *PodRef 的索引，供把 Hubble flow
// 里的 Pod 名还原成引擎求值需要的完整身份（IP、标签、命名端口）。
func indexPods(pods []replay.PodRef) map[string]*replay.PodRef {
	idx := make(map[string]*replay.PodRef, len(pods))
	for i := range pods {
		idx[pods[i].Namespace+"/"+pods[i].Name] = &pods[i]
	}
	return idx
}

// hubbleFlow 是 `hubble observe -o json` 一行里我们需要的部分。
type hubbleFlow struct {
	Flow struct {
		Verdict          string `json:"verdict"`
		TrafficDirection string `json:"traffic_direction"`
		Source           struct {
			Namespace string `json:"namespace"`
			PodName   string `json:"pod_name"`
		} `json:"source"`
		Destination struct {
			Namespace string `json:"namespace"`
			PodName   string `json:"pod_name"`
		} `json:"destination"`
		L4 struct {
			TCP *struct {
				DestinationPort int32 `json:"destination_port"`
			} `json:"TCP"`
		} `json:"l4"`
	} `json:"flow"`
}

// collectFlows 拿一个时间窗口内的真实 TCP flow。
//
// 这里 shell 出去调 hubble CLI，而不是 import
// github.com/cilium/cilium 直接调 Hubble 的 gRPC API：那个模块巨大，
// 会把 k8s.io 依赖和 go 指令一起拉高，而 go.mod 被钉死在 go 1.25.0、
// k8s.io/* v0.35.0（CLAUDE.md §8）。CLI 的 JSON 输出是稳定公开接口，
// 换 gRPC client 不值当为它冒版本联动的风险。
//
// 用 --follow 加超时主动收集，而不是事后用 --last/--since 翻 Hubble
// 的历史缓冲区：这个环境里好几路流量并发，缓冲区在没有事件驱动的
// 情况下按固定条数淘汰，等测试跑到收集这一步时，早的样本可能已经被
// 冲掉了。跟着流量实时收，才能保证窗口内看到的就是窗口内发生的。
func collectFlows(t *testing.T, addr string, window time.Duration) []hubbleFlow {
	t.Helper()

	if _, err := exec.LookPath("hubble"); err != nil {
		t.Fatalf("hubble CLI not found in PATH: %v (install it, or run test/conformance/setup.sh up)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()

	//nolint:gosec // G204: addr comes from DISTILL_CONFORMANCE_HUBBLE (an operator-set
	// env var) or the documented localhost default, never from request input; and
	// exec.CommandContext never invokes a shell, so there is no injection surface.
	cmd := exec.CommandContext(ctx, "hubble", "observe",
		"--server", addr, "-o", "json", "--protocol", "tcp", "--follow")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("open hubble stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hubble observe (server=%s): %v", addr, err)
	}

	var flows []hubbleFlow
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var hf hubbleFlow
		if err := json.Unmarshal(scanner.Bytes(), &hf); err != nil {
			continue // 非 flow 行（心跳/告警之类），跳过。
		}
		f := hf.Flow
		if f.Source.PodName == "" || f.Destination.PodName == "" || f.L4.TCP == nil {
			continue
		}
		flows = append(flows, hf)
	}
	// 采集少了 flow 只会让"一致条数"变小，不会让任何用例判错——结果比现实
	// 好看，是最危险的失败方向。所以流被提前掐断必须报出来：ctx 超时是本函数
	// 唯一的正常收尾方式，此时管道随进程一起关闭，那个错误不算故障；ctx 还
	// 活着却读出错，说明 hubble 中途死了，这一轮采集不可信。
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		t.Fatalf("hubble flow stream ended early: %v", err)
	}
	// 进程是被 ctx 超时杀掉的，Wait() 在这种收尾方式下报错是预期行为，
	// 不代表采集失败。真正的失败（hubble 连不上 relay 等）在 Start()
	// 阶段或者压根不产生任何 flow 时就会暴露出来。
	_ = cmd.Wait()

	return flows
}

// flowsFor 挑出属于某个期望用例的 flow：按 (源 ns, 目的 ns, 目的端口)
// 匹配，方向不作为过滤条件——同一个连接会在 ingress 侧和 egress 侧
// 各留一份记录（见下面 effectiveVerdict 的注释），两侧都得收进来。
func flowsFor(flows []hubbleFlow, c expectedCase) []hubbleFlow {
	var out []hubbleFlow
	for _, hf := range flows {
		f := hf.Flow
		if f.Source.Namespace == c.sourceNamespace &&
			f.Destination.Namespace == c.destNamespace &&
			f.L4.TCP.DestinationPort == c.port {
			out = append(out, hf)
		}
	}
	return out
}

// effectiveVerdict 把一组 flow 折成一个判定：任一方向出现过 DROPPED
// 就是 DENY，否则才是 ALLOW。
//
// 不能取多数：一条被 egress 策略拦下的连接，在被拦之前可能已经在别的
// 方向上转发过大量心跳（DNS 解析、之前放行时段的连接），多数投票会把
// 它读成"通的"。egress 拒绝这一刻起，这条连接对应用来说就是断的，
// 不管它此前转发过多少包。
func effectiveVerdict(flows []hubbleFlow) replay.Verdict {
	for _, hf := range flows {
		if hf.Flow.Verdict == "DROPPED" {
			return replay.VerdictDeny
		}
	}
	return replay.VerdictAllow
}

// evaluateCase 用引擎对某个用例下观测到的每一对 (源 Pod, 目的 Pod) 求值。
//
// 同一个 namespace 里可能有多个 Pod（比如 payment/api 两个副本），
// 理论上它们对同一条策略的判定应该完全一致——如果不一致，说明夹具或
// 引擎有本测试没预料到的分支，直接报错比悄悄取多数更安全。
func evaluateCase(
	t *testing.T, evaluator *replay.Evaluator, clusterID string,
	podIndex map[string]*replay.PodRef, flows []hubbleFlow,
) replay.Verdict {
	t.Helper()

	seen := map[string]bool{}
	var verdict replay.Verdict
	set := false

	for _, hf := range flows {
		f := hf.Flow
		key := f.Source.Namespace + "/" + f.Source.PodName + "->" +
			f.Destination.Namespace + "/" + f.Destination.PodName
		if seen[key] {
			continue
		}
		seen[key] = true

		src := podIndex[f.Source.Namespace+"/"+f.Source.PodName]
		dst := podIndex[f.Destination.Namespace+"/"+f.Destination.PodName]
		if src == nil || dst == nil {
			t.Fatalf("flow references a pod missing from the cluster snapshot: %s or %s",
				f.Source.Namespace+"/"+f.Source.PodName, f.Destination.Namespace+"/"+f.Destination.PodName)
		}

		decision := evaluator.Evaluate(replay.Flow{
			Source:   replay.Endpoint{ClusterID: clusterID, IP: src.IP, Pod: src},
			Dest:     replay.Endpoint{ClusterID: clusterID, IP: dst.IP, Pod: dst},
			Protocol: replay.ProtocolTCP,
			Port:     f.L4.TCP.DestinationPort,
		})

		if !set {
			verdict, set = decision.Verdict, true
			continue
		}
		if decision.Verdict != verdict {
			t.Fatalf("engine disagreed with itself across pod pairs for the same case: %s got %s, earlier pair got %s",
				key, decision.Verdict, verdict)
		}
	}
	return verdict
}
