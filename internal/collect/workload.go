package collect

// ownerRef 是一个控制器引用。
type ownerRef struct {
	Kind string
	Name string
}

// empty 报告该引用是否为空。
func (o ownerRef) empty() bool { return o.Kind == "" && o.Name == "" }

// kindReplicaSet 是唯一需要多跳一层才能到达顶层控制器的中间类型。
const kindReplicaSet = "ReplicaSet"

// resolveWorkload 沿 ownerRef 链把 Pod 解析到顶层控制器。
//
// 只处理 ReplicaSet 这一跳：StatefulSet / DaemonSet / Job 直接拥有 Pod，
// 本身就是顶层。Job → CronJob 还差一跳，但本轮不采 CronJob，
// 于是 Job 就是它能给出的最顶层答案 —— 这是一个已知的、有界的不完整，
// 不是一次解析失败。
//
// 第二个返回值为 false 表示引用指向一个不在索引里的 ReplicaSet。
// 这时返回的仍是那个 ReplicaSet 本身：给一个会随每次发布改名的主体，
// 好过给一个空值让上层以为这是一个裸 Pod。
func resolveWorkload(namespace string, podOwner ownerRef, rsOwners map[string]ownerRef) (ownerRef, bool) {
	if podOwner.empty() {
		// 裸 Pod。没有可解析的链条，不是解析失败。
		return ownerRef{}, true
	}
	if podOwner.Kind != kindReplicaSet {
		return podOwner, true
	}

	top, ok := rsOwners[namespace+"/"+podOwner.Name]
	if !ok || top.empty() {
		return podOwner, false
	}
	return top, true
}
