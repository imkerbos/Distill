package response

import (
	"encoding/json"
	"net/http"
)

// contentType 固定为 JSON，前端无需按响应猜格式。
const contentType = "application/json; charset=utf-8"

// Envelope 是所有响应的统一外层结构。
type Envelope struct {
	// Code 为 0 表示成功。
	Code Code `json:"code"`
	// Msg 是可直接展示给用户的文案。
	Msg string `json:"msg"`
	// Data 在失败时为 null。
	Data any `json:"data"`
}

// WriteOK 写出一个成功响应。
func WriteOK(w http.ResponseWriter, data any) {
	write(w, http.StatusOK, Envelope{Code: CodeOK, Msg: CodeOK.Message(), Data: data})
}

// WriteBusiness 写出一个业务级失败：HTTP 仍为 200，错误由 code 表达。
//
// 业务失败不该计入服务错误率 —— 查询一个不存在的 ID 不是服务出了问题。
func WriteBusiness(w http.ResponseWriter, code Code) {
	write(w, http.StatusOK, Envelope{Code: code, Msg: code.Message(), Data: nil})
}

// WriteInvalid 写出一个带校验详情的业务级失败。
//
// detail 只接受**本平台自己构造的**校验错误文案 —— 调用方应当从
// registry.InvalidError.Detail 取值，永不传 err.Error()：后者可能因为
// 错误包装带上第三方库（YAML/JSON 解析器等）的原始报错文本。这条边界
// 由类型强制，不是靠约定：InvalidError 把「可以回传的文案」（Detail）
// 与「只进日志的原因」（Cause）分成两个字段，要泄露第三方文本必须有人
// 主动把它写进 Detail，那在 review 里是看得见的一行。
//
// 与 WriteBusiness 分开而不是给它加参数：调用点必须显式选择「我要回传详情」，
// 否则某天有人把一个 driver error 顺手塞进去，而 code review 看不出区别。
func WriteInvalid(w http.ResponseWriter, detail string) {
	msg := detail
	if msg == "" {
		msg = CodeInvalidParam.Message()
	}
	write(w, http.StatusOK, Envelope{Code: CodeInvalidParam, Msg: msg, Data: nil})
}

// WriteSystem 写出一个系统性失败，保留真实的 HTTP 状态码。
//
// 协议层与系统层的失败必须能被网关、负载均衡与 APM 统计到；
// 一律返回 200 等于把这一层可观测性关掉。
func WriteSystem(w http.ResponseWriter, status int, code Code) {
	write(w, status, Envelope{Code: code, Msg: code.Message(), Data: nil})
}

// CodeRecorder 让中间件观察到 handler 实际写出的业务码。
//
// HTTP 状态码看不出业务失败（业务失败一律 200），日志里的 code 是唯一能
// 统计业务失败率的信号 —— 见 spec §4.3 的运维提示。
type CodeRecorder interface {
	// RecordCode 记录本次响应写出的业务码。
	RecordCode(Code)
}

// write 序列化并写出包络。
func write(w http.ResponseWriter, status int, env Envelope) {
	if rec, ok := w.(CodeRecorder); ok {
		rec.RecordCode(env.Code)
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	// 编码失败时响应头已发出，无法再改状态码；错误留给上层的日志中间件记录。
	_ = json.NewEncoder(w).Encode(env)
}
