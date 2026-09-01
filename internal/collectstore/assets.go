package collectstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// observedPod 是一次快照里与本轮有关的那几列。
//
// 只取用得上的列，不回放整行（安全规范 §20 / §35）。
type observedPod struct {
	namespace string
	// name 是 Pod 名。安全发现要点名裸奔的那几个 Pod，只报计数说不清该去看谁。
	name   string
	labels map[string]string
	// scrapeAnnotations 是这个 Pod 自己声明的 metrics 抓取意愿，
	// METRICS_SCRAPE Baseline 依据的一半（design doc 2026-08-18 §3）。
	scrapeAnnotations map[string]string
	hostNetwork       bool
	// namedPorts 是这个 Pod 声明的命名容器端口。podRefOf 靠它把解出来的主体
	// 补成求值引擎认的 replay.PodRef，命名端口规则才解析得出具体端口号——
	// 缺了它，一条合法的规则在这里只会判 NAMED_PORT_UNRESOLVED。
	namedPorts []snapshot.NamedPort
	// probePorts 是 kubelet 探针连接的端口（命名端口已在采集侧解析成数字），
	// KUBELET_PROBE Baseline 的唯一依据（design doc 2026-09-01）。
	probePorts []snapshot.NamedPort
	// probesCollected 表示这一行**采过**探针端口（probe_ports 列非 NULL）。
	//
	// 与 len(probePorts) == 0 是两件事：后者可能是"采过，这个 Pod 只有
	// exec 探针"，也可能是"这一行写在 migrations/000036 之前，根本没采过"。
	// 混成一个会让升级之后、下一次采集之前的每个 namespace 都被判成
	// 「不需要 KUBELET_PROBE」，从缺失清单里消失——一次数据缺口变成一次放行。
	probesCollected bool
}

// podKey 按 (namespace, name) 索引一次快照里的 Pod。
//
// 不含 cluster_id：一份 observedPod 只来自一次读取，而那次读取已经按
// cluster_id 收窄。跨集群混用要靠调用方不把两个集群的快照并进同一张表，
// 而本包一次读请求只描述一个集群。
type podKey struct {
	namespace string
	name      string
}

// observedPolicy 是一条落库的 NetworkPolicy：它在哪个命名空间、原文是什么。
type observedPolicy struct {
	namespace string
	manifest  string
}

// readIntervals 读出一个集群的全部身份区间，按地址分组。
//
// 按地址取全部而不是只取覆盖某一刻的那些，理由见 described.intervals：
// identity.Resolve 与 subjectAt 都要靠一个地址的完整区间集合才答得准。
//
// 每一条查询都带 cluster_id（CLAUDE.md §4）：不同集群 Pod CIDR 可能重叠，
// 漏掉它会把另一个集群的 Pod 解释成本集群的，且不报错。查询走主键前缀
// (cluster_id, pod_ip)，不做全表扫描。
//
// 超出上限报错而不截断：截断会让一部分地址凭空解不出主体，于是那些连接
// 被算进 UNKNOWN —— 一个读取上限伪装成关于集群的观测结论。
func (r *Reader) readIntervals(
	ctx context.Context, clusterID string,
) (map[string][]identity.Interval, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT pod_ip, valid_from, valid_to, namespace, pod_name, pod_uid,
		        workload_kind, workload_name, host_network, in_mesh
		   FROM pod_identity_interval
		  WHERE cluster_id = ?
		  ORDER BY pod_ip, valid_from
		  LIMIT ?`,
		clusterID, maxIntervalRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read identity intervals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]identity.Interval{}
	total := 0
	for rows.Next() {
		iv := identity.Interval{ClusterID: clusterID}
		var validTo sql.NullTime
		if err := rows.Scan(&iv.PodIP, &iv.ValidFrom, &validTo, &iv.Identity.Namespace,
			&iv.Identity.PodName, &iv.Identity.PodUID, &iv.Identity.WorkloadKind,
			&iv.Identity.WorkloadName, &iv.Identity.HostNetwork, &iv.Identity.InMesh); err != nil {
			return nil, fmt.Errorf("collectstore: scan identity interval: %w", err)
		}
		// NULL 是"至今未关闭"，落回零值时刻 —— identity.Interval.Open() 认的
		// 就是这个。填一个"很远的未来"会让"还没结束"与"结束于 9999 年"
		// 变成同一个值（migrations/000012 的同一条理由）。
		if validTo.Valid {
			iv.ValidTo = validTo.Time
		}
		out[iv.PodIP] = append(out[iv.PodIP], iv)
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate identity intervals: %w", err)
	}
	if total > maxIntervalRows {
		// 不给操作者一个「缩小时间窗」或「缩短保留期」的建议：区间读取不带
		// 时间条件（见上），而**今天仓库里没有任何回收或老化区间行的代码**，
		// 所以那两个动作都不存在，指过去只会让人去找一个没有的开关。说清楚
		// 现状即可 —— 这个集群的六个读方法此刻全部不可用，处置在平台侧
		// （design doc 2026-08-17 §10）。
		return nil, fmt.Errorf(
			"collectstore: cluster %s holds more than %d identity intervals; refusing rather "+
				"than describing part of it — identity intervals are never recycled today, "+
				"so all reads for this cluster stay unavailable until retention exists",
			clusterID, maxIntervalRows)
	}
	return out, nil
}

// readPodsAt 读出锚点那一次采集看到的 Pod。
//
// 标签只存在于快照里 —— 区间表刻意不带标签（它存的是主体，不是主体的
// 属性）。策略覆盖率必须拿标签去比 selector，因此这一份从 observed_pod 取，
// 且取的是**覆盖那一刻的那次运行**，不是最新一次。
func (r *Reader) readPodsAt(ctx context.Context, d described) ([]observedPod, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT namespace, name, labels, scrape_annotations, host_network, named_ports, probe_ports
		   FROM observed_pod
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed pods: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []observedPod
	for rows.Next() {
		var (
			p             observedPod
			raw           []byte
			rawScrape     []byte
			rawNamedPorts sql.NullString
			rawProbePorts sql.NullString
		)
		if err := rows.Scan(&p.namespace, &p.name, &raw, &rawScrape, &p.hostNetwork,
			&rawNamedPorts, &rawProbePorts); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed pod: %w", err)
		}
		if err := json.Unmarshal(raw, &p.labels); err != nil {
			// 标签列坏了就是坏了，不当成"这个 Pod 没有标签"：后者会让它被
			// 判成没有被任何 selector 覆盖，于是一次列损坏显示成一个裸 Pod。
			return nil, fmt.Errorf("collectstore: decode pod labels of cluster %s: %w", d.clusterID, err)
		}
		// 同理不吞：坏掉的抓取声明当成"这个 Pod 没声明过"，会让一条本该
		// 生成的 metrics 放行规则静默消失，而症状是监控在下发之后中断。
		if err := json.Unmarshal(rawScrape, &p.scrapeAnnotations); err != nil {
			return nil, fmt.Errorf(
				"collectstore: decode pod scrape annotations of cluster %s: %w", d.clusterID, err)
		}
		// NULL 是迁移之前写下的行：那次采集根本没有落容器端口，与
		// "落了、这个 Pod 没有命名端口"不是同一件事，因此留 nil 而不是
		// 报错或补一个空数组（migrations/000032）。
		if rawNamedPorts.Valid {
			if err := json.Unmarshal([]byte(rawNamedPorts.String), &p.namedPorts); err != nil {
				return nil, fmt.Errorf(
					"collectstore: decode pod named ports of cluster %s: %w", d.clusterID, err)
			}
		}
		// NULL 同理：迁移之前写下的行没有采过探针，与「采过、这个 Pod
		// 没有走网络的探针」不是同一件事——后者是 KUBELET_PROBE 的
		// NotApplicable 一档，前者不是（migrations/000036）。
		p.probesCollected = rawProbePorts.Valid
		if rawProbePorts.Valid {
			if err := json.Unmarshal([]byte(rawProbePorts.String), &p.probePorts); err != nil {
				return nil, fmt.Errorf(
					"collectstore: decode pod probe ports of cluster %s: %w", d.clusterID, err)
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed pods: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d pods at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// readPoliciesAt 读出锚点那一次采集看到的 NetworkPolicy 原文。
func (r *Reader) readPoliciesAt(ctx context.Context, d described) ([]observedPolicy, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT namespace, manifest
		   FROM observed_network_policy
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []observedPolicy
	for rows.Next() {
		var p observedPolicy
		if err := rows.Scan(&p.namespace, &p.manifest); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed policy: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed policies: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d network policies at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// readNamespacesAt 读出锚点那一次采集看到的命名空间与它们的标签。
//
// 标签是 namespaceSelector 的判据，缺了它，一条按 namespaceSelector 放行的
// 规则会被判成没有命中，于是一条现网通着的连接显示成会被拦断 —— 那正是
// 会变成一条错误"收紧"建议的方向。同样取覆盖那一刻的那次运行。
func (r *Reader) readNamespacesAt(ctx context.Context, d described) ([]replay.NamespaceRef, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT name, labels
		   FROM observed_namespace
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed namespaces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []replay.NamespaceRef
	for rows.Next() {
		ns := replay.NamespaceRef{ClusterID: d.clusterID}
		var raw []byte
		if err := rows.Scan(&ns.Name, &raw); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed namespace: %w", err)
		}
		if err := json.Unmarshal(raw, &ns.Labels); err != nil {
			// 与 Pod 标签同一条理由：坏掉的列不当成"这个命名空间没有标签"，
			// 后者会让一条按 namespaceSelector 写的规则安静地判成不命中。
			return nil, fmt.Errorf(
				"collectstore: decode namespace labels of cluster %s: %w", d.clusterID, err)
		}
		out = append(out, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed namespaces: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d namespaces at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// readServicesAt 读出锚点那一次采集看到的 Service。
//
// 只取 Baseline 推导用得上的几列（安全规范 §20 / §35）：selector 是 DNS
// 规则的 peer，ports 里的 targetPort 是健康检查规则的端口。ClusterIP 不取
// —— 它只供展示，且恰恰是最容易被误当成 peer 的那个值（NetworkPolicy 的
// peer 只能是 selector 或 ipBlock）。
func (r *Reader) readServicesAt(ctx context.Context, d described) ([]snapshot.Service, error) {
	rows, err := r.db.QueryContext(ctx,
		// service_type 一并取回：它是 LB_HEALTH_CHECK 这一类适不适用的判据
		// 之一。入口暴露对象在快照里本轮只含 Kind=Ingress，只看它会漏掉
		// type=LoadBalancer 的 Service —— 那种 namespace 一样有健康检查流量，
		// 一样会被 default-deny 打断，而把它判成"不适用"会让门禁放行一次
		// 入口中断（design doc 2026-08-18-baseline-applicability §4.1）。
		//
		// lb_ingress_ips / lb_source_ranges 一并取回：它们是判定这个入口
		// 面向公网还是只在 VPC 内的依据（design doc 2026-08-28 §2），
		// 推导层要拿它们分流，不是只供展示的两列。
		`SELECT namespace, name, service_type, selector, ports, lb_ingress_ips, lb_source_ranges
		   FROM observed_service
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed services: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []snapshot.Service
	for rows.Next() {
		s := snapshot.Service{ClusterID: d.clusterID}
		var selector, ports []byte
		var ingressIPs, sourceRanges sql.NullString
		if err := rows.Scan(&s.Namespace, &s.Name, &s.Type, &selector, &ports,
			&ingressIPs, &sourceRanges); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed service: %w", err)
		}
		// 坏掉的列整体报错，不当成"这个 Service 没有 selector"：后者会让
		// DNS 的 Baseline 悄悄推导不出来，于是一条本来齐备的必备规则显示成
		// 缺失，而缺失清单正是这份报告最该说准的那一栏。
		if err := json.Unmarshal(selector, &s.Selector); err != nil {
			return nil, fmt.Errorf(
				"collectstore: decode service selector of cluster %s: %w", d.clusterID, err)
		}
		if err := json.Unmarshal(ports, &s.Ports); err != nil {
			return nil, fmt.Errorf(
				"collectstore: decode service ports of cluster %s: %w", d.clusterID, err)
		}
		// NULL 是迁移之前写下的行：那次采集根本没有落暴露信息，与
		// "落了、这个 Service 没有 LoadBalancer 入口"不是同一件事，因此
		// 留 nil 而不是报错或补一个空数组（migrations/000032）。
		if ingressIPs.Valid {
			if err := json.Unmarshal([]byte(ingressIPs.String), &s.LoadBalancerIngressIPs); err != nil {
				return nil, fmt.Errorf(
					"collectstore: decode service lb ingress ips of cluster %s: %w", d.clusterID, err)
			}
		}
		if sourceRanges.Valid {
			if err := json.Unmarshal([]byte(sourceRanges.String), &s.LoadBalancerSourceRanges); err != nil {
				return nil, fmt.Errorf(
					"collectstore: decode service lb source ranges of cluster %s: %w", d.clusterID, err)
			}
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed services: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d services at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// readEndpointsAt 读出锚点那一次采集看到的 Service 后端地址。
//
// 后端非空是 DNS Baseline 的前置条件：一个存在但没有后端的 kube-dns
// 会生成一条指向空集的放行规则，看起来齐备、实际什么都没放行。
func (r *Reader) readEndpointsAt(ctx context.Context, d described) ([]snapshot.Endpoints, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT namespace, name, addresses
		   FROM observed_endpoints
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed endpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []snapshot.Endpoints
	for rows.Next() {
		e := snapshot.Endpoints{ClusterID: d.clusterID}
		var addresses []byte
		if err := rows.Scan(&e.Namespace, &e.Name, &addresses); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed endpoints: %w", err)
		}
		if err := json.Unmarshal(addresses, &e.Addresses); err != nil {
			return nil, fmt.Errorf(
				"collectstore: decode endpoint addresses of cluster %s: %w", d.clusterID, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed endpoints: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d service backends at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// readGatewaysAt 读出锚点那一次采集看到的入口暴露对象。
//
// 一个后端 Service 一行（migrations/000009）：健康检查 Baseline 是按后端
// Service 数的，按 Ingress 去重会让同一个 Ingress 的其余后端拿不到规则。
func (r *Reader) readGatewaysAt(ctx context.Context, d described) ([]snapshot.Gateway, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT namespace, name, backend_service, gateway_kind
		   FROM observed_gateway
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed gateways: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []snapshot.Gateway
	for rows.Next() {
		g := snapshot.Gateway{ClusterID: d.clusterID}
		if err := rows.Scan(&g.Namespace, &g.Name, &g.BackendService, &g.Kind); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed gateway: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed gateways: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d ingress objects at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// parsePolicies 把落库的策略原文解析成求值引擎要的对象。
//
// 命名空间**以表列为准**，覆盖 manifest 里的 metadata：那一列是采集当时写下的
// 事实，manifest 只是原文证据，两者不一致时该信前者。这一步不能省 —— 求值
// 引擎按 policy.Namespace 决定这条策略选中谁，一个空的命名空间会让整条策略
// 去选另一批 Pod。
//
// 解析不了的原文整体报错，**不跳过**：跳过一条读不懂的策略，它覆盖的 Pod
// 会被当成没有任何策略管辖，于是一次解析失败变成一句"这些连接没人管"。
func parsePolicies(policies []observedPolicy) ([]networkingv1.NetworkPolicy, error) {
	out := make([]networkingv1.NetworkPolicy, 0, len(policies))
	for _, p := range policies {
		var np networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(p.manifest), &np); err != nil {
			return nil, fmt.Errorf(
				"collectstore: a stored network policy in namespace %s cannot be parsed: %w", p.namespace, err)
		}
		np.Namespace = p.namespace
		out = append(out, np)
	}
	return out, nil
}

// selectorsByNamespace 把策略的 podSelector 按命名空间分组。
func selectorsByNamespace(policies []networkingv1.NetworkPolicy) (map[string][]labels.Selector, error) {
	out := map[string][]labels.Selector{}
	for i := range policies {
		sel, err := metav1.LabelSelectorAsSelector(&policies[i].Spec.PodSelector)
		if err != nil {
			return nil, fmt.Errorf(
				"collectstore: a stored network policy in namespace %s carries an unusable pod selector: %w",
				policies[i].Namespace, err)
		}
		out[policies[i].Namespace] = append(out[policies[i].Namespace], sel)
	}
	return out, nil
}

// subjectAt 回答一条连接的一端在被描述的那个窗口里属于谁。
//
// 主体一律由区间表按时刻解出，**不用来源自己报的那份身份**：Hubble 的流量
// 自带 Pod 标签而 VPC flow logs 只有地址，区间表正是为了让两者得到同一种
// 答案才建的（design doc §5）。来源报的那份留在事实层里做出处，不参与这里
// 的判断。
//
// 解出来之后再问一句"这个归属在整个窗口里稳不稳"：事实层按窗口存连接、
// 不带逐条时间戳，因此这一批连接只能用窗口起点解释。地址在窗口中途换过
// 主体时，起点那个答案对窗口后半段是错的 —— 而它答得出、不报错。这时
// 一律降级成 AMBIGUOUS：归属在这个窗口里本来就不唯一，与解析器对重叠
// 区间的处置是同一句话（identity.Resolve）。
func (d described) subjectAt(ep flow.Endpoint) (identity.Identity, identity.Outcome) {
	ivs := d.intervals[ep.IP]
	subject, outcome := identity.Resolve(ivs, d.at)
	if outcome == identity.OutcomeResolved && !stableAcross(ivs, d.window) {
		return identity.Identity{}, identity.OutcomeAmbiguous
	}
	return subject, outcome
}

// stableAcross 报告这个地址的归属在窗口内有没有换过手。
//
// 判据是"有没有任何区间的端点落在窗口内部"：一次交接必然在窗口中间留下
// 一个起点或一个终点。落在边界上的不算 —— 区间是左闭右开的，窗口也是，
// 恰好在 window.From 开始的区间覆盖的正是整个窗口的开头。
func stableAcross(intervals []identity.Interval, w flow.Window) bool {
	for _, iv := range intervals {
		if iv.ValidFrom.After(w.From) && iv.ValidFrom.Before(w.To) {
			return false
		}
		if iv.Open() {
			continue
		}
		if iv.ValidTo.After(w.From) && iv.ValidTo.Before(w.To) {
			return false
		}
	}
	return true
}

// confidenceOf 把窗口完整度传导成判定的可信度（design doc §4）。
//
// 完整度不是 COMPLETE 就是 DEGRADED：观测不全时给出的每一句话，与观测完整
// 时的同一句话含义不同。verdict 与 confidence 始终是两个字段，这里只填后者。
//
// 它只回答"这段观测本身可不可信"。窗口完整时可信度改由求值引擎给出 ——
// mesh 与 CCNP 是另外两条降级理由，与漏没漏采无关，见 replay.confidenceFor。
func confidenceOf(c flow.Completeness) replay.Confidence {
	if c == flow.CompletenessComplete {
		return replay.ConfidenceTrusted
	}
	return replay.ConfidenceDegraded
}

// readNodeIPsAt 读出锚点那一刻各节点的内部地址。
//
// 用途只有一个：判断一个落在登记 node 网段里的地址**到底是不是节点**。
//
// 登记的 node_cidr 是一个网段，而集群节点通常只占其中一部分——同子网里还有
// 数据库、跳板机、别的集群的节点。按网段判，这些机器全被算成"集群内"，平台
// 于是期待一个 Pod 主体、找不到就报 SNAPSHOT_MISSING，而运维照着这个原因去
// 查"哪次采集漏了快照"，根本没有这回事。UAT 实测：node 网段里 29 个出现过的
// 地址，15 个是节点、14 个不是，而那 14 个是 SNAPSHOT_MISSING 的主体。
//
// **节点清单是采出来的事实，不是人填的登记**，因此它比收窄 node_cidr 更可靠：
// 网段要有人去维护，而维护会漂。
func (r *Reader) readNodeIPsAt(ctx context.Context, d described) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT internal_ips FROM observed_node
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed node: %w", err)
		}
		// 坏掉的列整体报错，不当成"这个节点没有地址"：后者会让这个节点的
		// 地址退回按网段判，也就是退回这段代码要修的那个缺陷本身。
		var ips []string
		if err := json.Unmarshal(raw, &ips); err != nil {
			return nil, fmt.Errorf(
				"collectstore: decode node internal ips of cluster %s: %w", d.clusterID, err)
		}
		for _, ip := range ips {
			out[ip] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed nodes: %w", err)
	}
	return out, nil
}
