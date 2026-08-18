package httpapi

import "net/http"

// MaxRequestBytes 是任何一个请求体被允许读到的最大字节数。
//
// 取值有依据，不是一个顺手写下的整数：
//
//   - 全部端点里只有一个请求体长度可变 —— POST
//     /clusters/{clusterID}/policy-imports 的 yaml 字段，内容是一份
//     NetworkPolicy 清单。仓库里现存最大的一份（internal/registry 的解析
//     用例）是 469 字节。
//   - Kubernetes 自己存不下超过约 1.5 MiB 的单个对象（etcd 默认的请求
//     尺寸上限）。大于那个尺寸的 NetworkPolicy 在集群里根本不可能存在，
//     也就没有被导入的必要。
//   - 其余四个带请求体的端点（登录、集群登记、Git 绑定、人工覆盖）都只是
//     几个短字符串，量级在百字节。
//
// 1 MiB 同时落在这两条线之间：低于 Kubernetes 那条天花板，又高出任何一份
// 真实清单三个数量级，正常调用方碰不到它；同时把 encoding/json 单次请求的
// 工作量钉死在一个解析器毫无压力的尺寸上（规范 §24「超大 JSON」）。
const MaxRequestBytes int64 = 1 << 20

// MaxAgentRequestBytes 是 agent 子树的上限。
//
// 这条子树的报文与人的子树差三个数量级，一个数字管不住两边：
//
//   - 一次资产上报是整个集群的 Pod / Service / Endpoints / NetworkPolicy。
//     UAT 实测 650 个 Pod，约 200 KB。
//   - 一次流量摄入是那个窗口里按 (src, dst, proto, port) 去重后的连接。
//     同一个集群量级是 1–2 万条，JSON 约 1–2 MB —— 已经过不去 1 MiB。
//
// 8 MiB 留了四倍余量，同时仍然远低于「一个解析器会难受」的尺寸。真正挡住
// 条数的是 maxIngestConnections（拒绝，不截断），本上限只是它的兜底：
// 一份声称两万条、实际塞了别的东西的报文，不该先把内存吃掉再被拒。
//
// **取值大不等于没有守卫。** 两条上限都必须存在 —— 少了字节数上限，一份
// 连条数字段都解不出来的报文会先把内存吃掉；少了条数上限，两万条以内但
// 每条都超长的报文会绕过它。
const MaxAgentRequestBytes int64 = 8 << 20

// LimitRequestBody 限制每个请求体可被读取的字节数。
//
// 装成中间件而不是在每个 Decode 调用点各包一层：漏掉其中任何一个，
// 漏掉的那个就是攻击者会选的那个。新增一个读请求体的 handler 时，
// 它也不需要记得做这件事。
//
// **按子树装，不装在路由根部。** 根部只能取两条子树里小的那个上限，
// 而那会让资产推送与流量摄入过不去。代价是「新增一条子树忘了声明」有了
// 落脚点，由 TestEveryBodyTakingSubtreeIsAccountedFor 守着：那条用例走完
// 路由表，认不出的子树就红。
//
// 中间件**只会收紧、不会放宽**：内层先包一个小上限，外层再包一个大的，
// 读到小的那个仍然报错。因此同一条链上不要装两次。
//
// 超限的表现是 Decode 返回错误，于是走到与「请求体不是合法 JSON」完全
// 相同的那条分支：400 + 20001。这里刻意不为它单开一个 413 —— 那要改动
// 全部五个调用点，而上限高出任何真实请求体三个数量级，正常调用方到不了
// 这条路径。哪天需要区分，改的是 handler，不是这里。
//
// 客户端拿到的始终是本平台自己的响应包络：http.MaxBytesError 的文本
// （"http: request body too large"）只停留在 handler 内部的 err 上，
// 不会被回传（规范 §22）。
func LimitRequestBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 真实的 net/http server 保证 r.Body 非 nil，但中间件也会被
			// 直接构造的请求打到；MaxBytesReader 包住 nil 会在第一次读时 panic。
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}
