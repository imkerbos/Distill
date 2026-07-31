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

	// CodeInvalidParam 表示请求参数校验失败。
	CodeInvalidParam Code = 20001
	// CodeNotFound 表示请求的资源不存在。
	CodeNotFound Code = 20002

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
	CodeInvalidParam:          "请求参数不合法",
	CodeNotFound:              "请求的资源不存在",
	CodeInternal:              "服务内部错误",
	CodeDependencyUnavailable: "依赖服务暂时不可用",
}

// Message 返回该码对应的用户可见文案。
func (c Code) Message() string {
	return messages[c]
}

// AllCodes 返回全部已登记的错误码，供测试校验文案完整性。
func AllCodes() []Code {
	out := make([]Code, 0, len(messages))
	for c := range messages {
		out = append(out, c)
	}
	return out
}
