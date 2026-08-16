// Package response 定义统一响应包络与错误码。
package response

// Code 是业务错误码。0 表示成功。
//
// 分段而非连号：后续增加模块时无需重排既有取值，
// 前端与告警规则里写死的数字不会失效。
type Code int

const (
	// CodeOK 表示成功。
	CodeOK Code = 0

	// CodeInvalidCredentials 表示用户名或密码不正确。
	CodeInvalidCredentials Code = 10001
	// CodeSessionExpired 表示会话已过期。
	CodeSessionExpired Code = 10002
	// CodeUnauthenticated 表示请求未携带有效会话。
	CodeUnauthenticated Code = 10003
	// CodeForbidden 表示会话有效，但该账号的角色不足以执行这次操作。
	//
	// 与 10002/10003 分开是必需的，不是细分：前端对「重新登录」与
	// 「你不能做这件事」的处置完全相反，共用一个码会让一次权限不足
	// 变成一次退出登录，而重新登录之后结果一模一样。
	CodeForbidden Code = 10004

	// CodeInvalidParam 表示请求参数校验失败。
	CodeInvalidParam Code = 20001
	// CodeNotFound 表示请求的资源不存在。
	CodeNotFound Code = 20002
	// CodeRateLimited 表示请求过于频繁，被限流拒绝。
	CodeRateLimited Code = 20003
	// CodeNoCollectionRun 表示该集群已注册，但还没有过任何一次资产采集。
	//
	// 与 20002 分开是必需的，不是细分，理由同 10004：真实读取端对一个
	// 不存在的集群同样查不到采集行，两者共用一个码，一次集群 ID 拼写错误
	// 就会得到"还没有采集记录"，于是操作者去查采集器为什么没跑，
	// 而真正的原因是他打错了字。
	CodeNoCollectionRun Code = 20004

	// CodeInternal 表示服务端内部错误。
	CodeInternal Code = 50001
	// CodeDependencyUnavailable 表示依赖不可用。
	CodeDependencyUnavailable Code = 50002
)

// messages 是码到用户可见文案的映射。
//
// 文案是固定的：内部错误一律回同一句话，真实原因带 request_id 进日志。
// 把错误细节回传给调用方，等于顺着 API 把堆栈和文件路径送出去。
//
//nolint:gosec // G101 false positive: these are user-facing message strings, not credentials.
var messages = map[Code]string{
	CodeOK:                    "ok",
	CodeInvalidCredentials:    "用户名或密码不正确",
	CodeSessionExpired:        "会话已过期，请重新登录",
	CodeUnauthenticated:       "请先登录",
	CodeForbidden:             "当前账号没有执行该操作的权限",
	CodeInvalidParam:          "请求参数不合法",
	CodeNotFound:              "请求的资源不存在",
	CodeRateLimited:           "请求过于频繁，请稍后再试",
	CodeNoCollectionRun:       "该集群还没有过资产采集记录",
	CodeInternal:              "服务内部错误",
	CodeDependencyUnavailable: "依赖服务暂时不可用",
}

// Message 返回该码对应的用户可见文案。
//
// 未登记的码退回内部错误文案而不是空串：一个没有任何说明的错误提示，
// 对用户和排障的人都毫无价值，而空 msg 的成因（漏登记、码被删）
// 恰恰是最难在代码评审里看出来的。
func (c Code) Message() string {
	if m, ok := messages[c]; ok {
		return m
	}
	return messages[CodeInternal]
}

// registered 列出全部已登记的 Code 常量。
//
// 有意手工维护、与 messages 分离：如果从 messages 反推这份列表，
// 一个漏配文案的码永远不会出现在列表里，也就永远逃过校验它的测试。
var registered = []Code{
	CodeOK,
	CodeInvalidCredentials,
	CodeSessionExpired,
	CodeUnauthenticated,
	CodeForbidden,
	CodeInvalidParam,
	CodeNotFound,
	CodeRateLimited,
	CodeNoCollectionRun,
	CodeInternal,
	CodeDependencyUnavailable,
}

// AllCodes 返回全部已登记的错误码，供测试校验文案完整性。
func AllCodes() []Code {
	out := make([]Code, len(registered))
	copy(out, registered)
	return out
}

// MessagedCodes 返回文案表中登记的全部码。
//
// 仅供测试校验它与 registered 保持一致 —— 两张表脱节时，
// 封闭枚举的保证就名存实亡。
func MessagedCodes() []Code {
	out := make([]Code, 0, len(messages))
	for c := range messages {
		out = append(out, c)
	}
	return out
}
