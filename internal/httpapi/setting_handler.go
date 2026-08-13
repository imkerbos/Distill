package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// settingPayload 是平台设置的请求体形状。
//
// 时长以秒 / 毫秒的整数表达，而不是 Go 的 time.Duration（纳秒）：库里的列
// 就是秒与毫秒（design doc 2026-08-13 §2），而 mysqlregistry.durationIn 对
// 除不尽的时长直接报错 —— 让 API 与列用同一个单位，操作者就不可能提交一份
// 「保存成功但被静默截断」的设置。多引入一种表示，迟早有人在换算处少乘
// 一个 1000，而症状是会话立即过期或超时保护被关掉，没有任何报错。
//
// 结论字段一个都没有：平台设置里本就不存在平台自己产生的断言。
type settingPayload struct {
	SessionTTLSeconds     int64  `json:"sessionTtlSeconds"`
	HTTPReadTimeoutMs     int64  `json:"httpReadTimeoutMs"`
	HTTPWriteTimeoutMs    int64  `json:"httpWriteTimeoutMs"`
	HTTPShutdownTimeoutMs int64  `json:"httpShutdownTimeoutMs"`
	SecretsBackend        string `json:"secretsBackend"`
	SecretsProject        string `json:"secretsProject"`
	SecretsPrefix         string `json:"secretsPrefix"`
	SecretsDir            string `json:"secretsDir"`
	GitVerifyTimeoutMs    int64  `json:"gitVerifyTimeoutMs"`
	// GitVerifyHostKeys 是 known_hosts 格式的原文，**只入不出**。
	//
	// 它在响应形状 settingView 里没有对应字段，那是刻意的：读取端点回的是
	// 指纹（design doc §1.3、规范 §19/§20）。一份设置的响应不能变成把写进去
	// 的东西再读回来的通道。
	GitVerifyHostKeys string `json:"gitVerifyHostKeys"`
}

// toSetting 把请求体转成领域对象。
func (p settingPayload) toSetting() registry.PlatformSetting {
	return registry.PlatformSetting{
		SessionTTL:          time.Duration(p.SessionTTLSeconds) * time.Second,
		HTTPReadTimeout:     time.Duration(p.HTTPReadTimeoutMs) * time.Millisecond,
		HTTPWriteTimeout:    time.Duration(p.HTTPWriteTimeoutMs) * time.Millisecond,
		HTTPShutdownTimeout: time.Duration(p.HTTPShutdownTimeoutMs) * time.Millisecond,
		SecretsBackend:      registry.SecretsBackend(p.SecretsBackend),
		SecretsProject:      p.SecretsProject,
		SecretsPrefix:       p.SecretsPrefix,
		SecretsDir:          p.SecretsDir,
		GitVerifyTimeout:    time.Duration(p.GitVerifyTimeoutMs) * time.Millisecond,
		GitVerifyHostKeys:   p.GitVerifyHostKeys,
	}
}

// settingView 是平台设置的响应形状。
//
// 与 settingPayload 是两个类型，而不是同一个结构体加一条「回传前记得把
// host key 清空」的约定：清空要靠每一处写响应的代码自觉，而漏掉的那一处
// 就是会被挑中的那一处。响应类型里**根本没有**承载原文的字段，于是
// 「设置响应回显了 host key」这件事在这一层写不出来（规范 §19、§20、§35）。
type settingView struct {
	SessionTTLSeconds     int64  `json:"sessionTtlSeconds"`
	HTTPReadTimeoutMs     int64  `json:"httpReadTimeoutMs"`
	HTTPWriteTimeoutMs    int64  `json:"httpWriteTimeoutMs"`
	HTTPShutdownTimeoutMs int64  `json:"httpShutdownTimeoutMs"`
	SecretsBackend        string `json:"secretsBackend"`
	SecretsProject        string `json:"secretsProject"`
	SecretsPrefix         string `json:"secretsPrefix"`
	SecretsDir            string `json:"secretsDir"`
	GitVerifyTimeoutMs    int64  `json:"gitVerifyTimeoutMs"`
	// GitVerifyHostKeysFingerprint 是当前 host key 原文的指纹；未配置时为空串。
	//
	// 指纹而非原文：host key 是平台连接策略仓库时的信任锚，而它能从后台改
	// （design doc §1.3）。操作者需要的是「现在装的还是不是我认得的那一份」，
	// 回答这个问题只需要指纹。
	GitVerifyHostKeysFingerprint string `json:"gitVerifyHostKeysFingerprint"`
}

// toSettingView 把一份设置转成响应形状。
func toSettingView(s registry.PlatformSetting) settingView {
	return settingView{
		SessionTTLSeconds:            int64(s.SessionTTL / time.Second),
		HTTPReadTimeoutMs:            int64(s.HTTPReadTimeout / time.Millisecond),
		HTTPWriteTimeoutMs:           int64(s.HTTPWriteTimeout / time.Millisecond),
		HTTPShutdownTimeoutMs:        int64(s.HTTPShutdownTimeout / time.Millisecond),
		SecretsBackend:               string(s.SecretsBackend),
		SecretsProject:               s.SecretsProject,
		SecretsPrefix:                s.SecretsPrefix,
		SecretsDir:                   s.SecretsDir,
		GitVerifyTimeoutMs:           int64(s.GitVerifyTimeout / time.Millisecond),
		GitVerifyHostKeysFingerprint: hostKeysFingerprint(s.GitVerifyHostKeys),
	}
}

// hostKeysFingerprint 返回 host key 原文的 SHA-256 指纹。
//
// 对整段原文取一次摘要，而不是逐条解析出每把公钥各算一个指纹：解析
// known_hosts 的规则（marker、通配符、哈希主机名）已经在 gitverify 里写过
// 一遍，抄第二份的那一天，只会是其中一处收紧了而另一处没有。这里要回答的
// 问题也不需要解析 —— 操作者看的是「当前装的还是不是我认得的那一份」，
// 而任何一个字节的改动都会让摘要变掉。
//
// 空值回空串而不是空串的摘要：为空是一种要在界面上被看见的状态
// （host key 为空时 gitverify.New 拒绝构造，于是一切校验都是 NOT_VERIFIED），
// 而一个看上去正常的指纹会把它藏起来。
func hostKeysFingerprint(hostKeys string) string {
	if hostKeys == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(hostKeys))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// handleGetSetting 读当前的平台设置。
//
// 每次都现读，不做进程内缓存：设置的读取路径整体是「按需读取」的
// （design doc §1.1），这个端点也不例外 —— 一个从缓存回答的设置页会在
// 别处改完设置后继续显示旧值。
func handleGetSetting(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := d.Registry.Setting(r.Context())
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, toSettingView(s))
	}
}

// handleUpdateSetting 整体替换平台设置。
//
// PUT 而非 PATCH：设置页保存的就是完整的一份，而部分更新会让审计的前后值
// 只描述「这次提交带上了哪几个字段」，追溯时读不出全貌
// （design doc §2、registry.Store.UpdateSetting）。
//
// 校验与审计都在 UpdateSetting 里，同事务：host key 是信任锚，而它能从
// 后台改（design doc §1.3）—— 那条审计行是事后唯一能回答「信任锚是谁在
// 什么时候换的」的东西，因此不能是「先写库、再补一条日志」。
//
// 回的是刚写下的那一份的视图，仍然只回指纹：操作者据此确认自己提交的
// host key 就是现在生效的那一份，而不必把原文再拿回来一次。
func handleUpdateSetting(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p settingPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			// 与集群、仓库端点同一处置：请求体不是合法 JSON 是协议层问题。
			response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
			return
		}
		s := p.toSetting()
		if err := d.Registry.UpdateSetting(r.Context(), actorOf(r), s); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, toSettingView(s))
	}
}
