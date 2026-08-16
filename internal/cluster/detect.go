package cluster

const (
	// meshInjectionLabel 是 Istio 的 namespace 级注入开关。
	meshInjectionLabel = "istio-injection"
	// meshRevisionLabel 是 Istio 的 revision 级注入开关。
	meshRevisionLabel = "istio.io/rev"
)

// meshSidecarContainers 是已知的 sidecar 容器名。
//
// 列全而非只认 istio-proxy：漏掉一种 sidecar 的后果是该 Pod 不被标记
// DEGRADED，于是一份失真的 L4 身份会被当作可信证据用来生成策略推荐 ——
// 这是"结果比现实好看"的方向，本平台的判定必须往反方向错。
var meshSidecarContainers = map[string]MeshSource{
	"istio-proxy":   MeshSourceIstioSidecar,
	"linkerd-proxy": MeshSourceLinkerdSidecar,
}

// MeshSource 是 mesh 判定的依据类别，封闭枚举。
//
// 与 Detail 分开：类别进统计口径，具体取值只用于向操作者解释单条判定。
// 合成一个自由文本字段会让"有多少 Pod 因为 sidecar 而降级"变成字符串匹配。
type MeshSource string

const (
	// MeshSourceNamespaceInjection 表示依据来自 namespace 的注入开关标签。
	MeshSourceNamespaceInjection MeshSource = "NS_INJECTION_LABEL"
	// MeshSourceNamespaceRevision 表示依据来自 namespace 的 revision 标签。
	MeshSourceNamespaceRevision MeshSource = "NS_REVISION_LABEL"
	// MeshSourceIstioSidecar 表示依据是 Pod 内存在 Istio sidecar 容器。
	MeshSourceIstioSidecar MeshSource = "ISTIO_SIDECAR"
	// MeshSourceLinkerdSidecar 表示依据是 Pod 内存在 Linkerd sidecar 容器。
	MeshSourceLinkerdSidecar MeshSource = "LINKERD_SIDECAR"
)

// MeshState 是 mesh 接入判定结果。
//
// 平台不管控 mesh，只检测：sidecar 会吃掉 L4 源身份，命中时上层必须把
// 判定标记为 DEGRADED，否则会基于失真身份生成错误的策略推荐。
type MeshState struct {
	// InMesh 表示该对象位于 service mesh 内。
	InMesh bool
	// Source 是判定依据的类别，InMesh 为 false 时为空。
	Source MeshSource
	// Detail 是依据的具体取值（容器名、revision 名），仅供解释单条判定。
	Detail string
}

// DetectNamespaceMesh 依据 namespace 标签判定是否启用了 sidecar 注入。
//
// 注意这判定的是 namespace 的注入配置，不是其中每个 Pod 的实际状态：
// Pod 可以单独关闭注入，也可能创建于开关打开之前。Pod 的实际状态一律
// 用 DetectPodMesh 从容器列表判定。
func DetectNamespaceMesh(labels map[string]string) MeshState {
	if labels[meshInjectionLabel] == "enabled" {
		return MeshState{InMesh: true, Source: MeshSourceNamespaceInjection, Detail: "enabled"}
	}
	if rev := labels[meshRevisionLabel]; rev != "" {
		return MeshState{InMesh: true, Source: MeshSourceNamespaceRevision, Detail: rev}
	}
	return MeshState{}
}

// DetectPodMesh 依据 Pod 的容器名列表判定是否注入了 sidecar。
//
// 取容器列表而非 namespace 标签：注入是否真的发生只有 Pod 自己知道，
// 按标签判定会把一个显式关闭了注入的 workload 误报成 DEGRADED，也会
// 漏掉标签打开之前就已存在的 Pod。
func DetectPodMesh(containerNames []string) MeshState {
	for _, n := range containerNames {
		if src, ok := meshSidecarContainers[n]; ok {
			return MeshState{InMesh: true, Source: src, Detail: n}
		}
	}
	return MeshState{}
}
