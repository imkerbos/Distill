package collect

import (
	"fmt"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// Classify 给一次采集结果里的每个 Pod 填上 IP 归属，并追加与登记不符时的告警。
//
// **与采集分开，是因为判定要看全 fleet 的网段**（design doc 2026-08-18 §3.4）：
// 判断一个 Pod IP 是否落在**别的**集群网段里，恰恰需要看得见别的集群。而在
// 推送式接入下，agent 只看得见自己那个集群 —— 把 fleet 网段下发给每一个被管
// 集群，等于把整个 fleet 的拓扑发出去。
//
// 于是分类挪到「拿到观测之后」这一步：PULL 模式由采集器调，PUSH 模式由平台
// 在收下时调，**同一份实现**。附带的好处是判定依据变了（新集群接入、网段
// 改了）时，平台可以拿原始观测重算历史；烧进上报数据里就再也纠正不了。
//
// **不修改入参**：重放要成立，「分类前的观测」必须还在。
//
// **不信任传进来的归属，一律重算。** 起初这里写的是「已经有归属就跳过」，
// 那是错的：PUSH 模式下这份观测来自被管集群里的 agent，跳过等于让 agent
// 自己声明的归属存活下来 —— 而归属决定一条流量被还原成哪个集群的主体，
// 正是平台绝不能外包出去的那个判断（design doc §3.4）。代价是同一份观测
// 跑两次会让告警翻倍，但真实路径上不存在这种调用：PULL 由采集器分一次，
// PUSH 由平台分一次，没有第三处。
func Classify(run snapshot.Run, reg *cluster.Registry) snapshot.Run {
	out := run

	pods := make([]snapshot.Pod, len(run.Observation.Pods))
	copy(pods, run.Observation.Pods)
	warnings := make([]snapshot.Warning, len(run.Observation.Warnings))
	copy(warnings, run.Observation.Warnings)

	for i := range pods {
		subject := fmt.Sprintf("%s/%s", pods[i].Namespace, pods[i].Name)
		got, ws := classifyPodIP(reg, run.Observation.ClusterID,
			pods[i].IP, subject, pods[i].HostNetwork)
		pods[i].IPScope = got.Scope
		pods[i].IPScopeReason = got.Reason
		warnings = append(warnings, ws...)
	}

	out.Observation.Pods = pods
	out.Observation.Warnings = warnings
	return out
}

// classifyPodIP 判定 Pod IP 的归属，并对与登记不符的情况产出告警。
//
// 这是 cluster.Registry 在生产路径上的调用点。它有两个作用，缺一不可：
// 把"注册表里的网段填错了"变成采集当时就能发现的事实，而不是等到求值
// 阶段以错误的集群归属表现出来；以及让分类器有一个删掉就会被测出来的
// 使用者 —— 一个没有调用点的守卫，其测试证明不了系统正确。
//
// hostNetwork 决定"哪一种归属才算对"。这不是一个可有可无的细分：
// hostNetwork Pod 用的就是它所在节点的地址，判成 NODE 是正确答案。
// 不区分的话，一个健康集群里的每一个 cilium / etcd / kube-apiserver /
// kube-proxy 都会各报一条"落在登记网段之外"—— 实测 kind 上 28 个 Pod
// 报了 12 条，全是误报。一条每次都在喊的告警会被整体忽略，
// 于是真正填错网段的那一次也一起被忽略了。
func classifyPodIP(
	reg *cluster.Registry, clusterID, ip, subject string, hostNetwork bool,
) (cluster.Classification, []snapshot.Warning) {
	if ip == "" {
		// 尚未分配 IP 的 Pod 不是异常，Phase 已经解释了它。
		return cluster.Classification{}, nil
	}

	got, err := reg.Classify(ip)
	if err != nil {
		return cluster.Classification{}, []snapshot.Warning{{
			Kind:    snapshot.WarningPodIPUnparsable,
			Subject: subject,
			Detail:  ip,
		}}
	}

	// 期望的归属取决于这个 Pod 用的是谁的网络栈。
	want := cluster.ScopePod
	if hostNetwork {
		want = cluster.ScopeNode
	}

	switch got.Scope {
	case cluster.ScopeAmbiguous:
		return got, []snapshot.Warning{{
			Kind:    snapshot.WarningPodIPAmbiguous,
			Subject: subject,
			Detail:  ip,
		}}
	case cluster.ScopeUnknown:
		return got, []snapshot.Warning{{
			Kind:    snapshot.WarningPodIPUnclassifiable,
			Subject: subject,
			Detail:  ip + " (" + string(got.Reason) + ")",
		}}
	case want:
		if got.ClusterID != clusterID {
			// 我们正从 clusterID 采集这个 Pod，而它的 IP 落在另一个集群
			// 的登记网段里。两份登记至少有一份是错的，而错的那份会让
			// 每一条涉及这个 IP 的流量都被还原成错误集群的主体。
			return got, []snapshot.Warning{{
				Kind:    snapshot.WarningPodIPOutsideCluster,
				Subject: subject,
				Detail:  ip + " falls in registered " + string(want) + " CIDR of " + got.ClusterID,
			}}
		}
		return got, nil
	default:
		// 归属不是这个 Pod 该有的那一类。普通 Pod 落在 NODE 网段、
		// 或 hostNetwork Pod 落在 POD 网段，都说明有一份登记是错的。
		return got, []snapshot.Warning{{
			Kind:    snapshot.WarningPodIPOutsideCluster,
			Subject: subject,
			Detail:  ip + " classified as " + string(got.Scope) + ", want " + string(want),
		}}
	}
}
