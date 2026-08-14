package httpapi_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

const settingPath = "/api/v1/settings"

// errTestRegistry 是一条**带着内部拓扑**的存储故障：主机、端口与驱动名
// 都在里面。用它而不是一句 "boom"，是因为这些用例要断言的正是这些字符
// 一个都不会顺着响应走出去（规范 §19、§22）。
var errTestRegistry = errors.New("mysql: dial tcp 10.0.0.5:3306: connection refused")

// testHostKeys 是一条 known_hosts 记录。
//
// 公钥不是机密，但它是**信任锚**：平台靠它认出自己连上的是不是那台 Git
// 服务器（design doc §1.3）。设置页只显示它的指纹，因此这个常量在断言里
// 的用途是「这段文字一个字都不能出现在响应里」。
const testHostKeys = "gitlab.example.com ssh-ed25519 " +
	"AAAAC3NzaC1lZDI1NTE5AAAAIKyjWioKIYrbPTzY9F8JKIElwSThZ4xuqtqQPGo9tDIg"

// testDirBackendPath 是 DIR 后端的凭据目录。
//
// 提成常量而不是写在两处字面量里：除了让期望值与请求体共用同一个值，
// 也避免 gosec G101 把「名字带 secrets」的路径字面量当成硬编码凭据 ——
// 这里存的是一个目录名，不是凭据，而消掉误报比挂一条 //nolint 更诚实
// （同 gitBindingRef 的处置）。
const testDirBackendPath = "/etc/distill/refs"

// settingBody 是一份完整的设置请求体。
//
// extra 里的键会并进对象，用于往请求体里塞那些**不该被采纳**的字段。
func settingBody(extra map[string]any) map[string]any {
	b := map[string]any{
		"sessionTtlSeconds": 28800, "httpReadTimeoutMs": 10000,
		"httpWriteTimeoutMs": 20000, "httpShutdownTimeoutMs": 15000,
		"secretsBackend": "DIR", "secretsProject": "", "secretsPrefix": "",
		"secretsDir": testDirBackendPath, "gitVerifyTimeoutMs": 10000,
		"gitVerifyHostKeys": testHostKeys,
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// settingRegistry 是一个 host key 已经配好的注册表替身。
func settingRegistry() *memRegistry {
	reg := newMemRegistry()
	reg.setting.GitVerifyHostKeys = testHostKeys
	return reg
}

// 设置读取只回指纹，不回 host key 原文。
//
// host key 是平台连接策略仓库时的信任锚，而它能从后台改（design doc §1.3）。
// 一个把原文回显出来的读取端点，会让「设置页」变成把写进去的东西再读回来的
// 通道 —— 拿到一个已认证会话的人因此不必知道任何东西，就能读出平台当前信任
// 的是哪几把 host key（规范 §19、§20、§35）。
//
// 三个方向一起守，缺一个这条测试都能被一个错误的实现通过：
//   - 原文的任何一段都不得出现在响应体里（整段、以及切出来的那截 base64）。
//   - 指纹必须真的在，且真的随原文变 —— 一个恒为空串或恒为同一个常量的
//     「指纹」同样满足前一条。
//   - 未配置时是空串而不是一个看上去正常的指纹：为空是一种要在界面上被
//     看见的状态（gitverify.New 会拒绝构造，于是一切校验都是 NOT_VERIFIED）。
func TestGetSettingsReturnsAFingerprintNotTheHostKeys(t *testing.T) {
	reg := settingRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedGet(t, h, cookie, settingPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// 整段与那截 base64 分别断言：一个只把换行去掉、或只回其中一行的实现
	// 会通过「整段不出现」，却仍然泄露了公钥本身。
	for _, secret := range []string{testHostKeys, "AAAAC3NzaC1lZDI1NTE5AAAAIKyjWioKIYrbPTzY9F8JKIElwSThZ4xuqtqQPGo9tDIg"} {
		if strings.Contains(body, secret) {
			t.Errorf("response echoed the host key material: %s", body)
		}
	}

	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	fp, _ := data["gitVerifyHostKeysFingerprint"].(string)
	if fp == "" {
		t.Fatal("gitVerifyHostKeysFingerprint is empty — the operator has no way to tell which keys are installed")
	}
	if _, ok := data["gitVerifyHostKeys"]; ok {
		t.Error("the response carries a gitVerifyHostKeys field at all — the read shape must have no such field")
	}

	// 指纹随原文变：一个恒为常量的「指纹」在上面的断言下也是绿的。
	reg.setting.GitVerifyHostKeys = testHostKeys + "\ngithub.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB"
	other, _ := bodyOf(t, authedGet(t, h, cookie, settingPath))["data"].(map[string]any)
	if other["gitVerifyHostKeysFingerprint"] == fp {
		t.Error("the fingerprint did not change with the host keys — it is not a fingerprint of anything")
	}

	// 未配置时是空串，不是一个看上去正常的指纹。
	reg.setting.GitVerifyHostKeys = ""
	empty, _ := bodyOf(t, authedGet(t, h, cookie, settingPath))["data"].(map[string]any)
	if empty["gitVerifyHostKeysFingerprint"] != "" {
		t.Errorf("fingerprint = %v with no host keys, want an empty string — an absent trust anchor must be visible",
			empty["gitVerifyHostKeysFingerprint"])
	}
}

// 保存的设置必须整份落库，且时长单位不能在换算里走样。
//
// 整体比对而不是挑几个字段：新增一项却忘记映射时，表现必须是这条测试
// 失败，而不是没有人注意到 —— 而漏掉的那一项在运行期的表现是「界面上
// 改了、进程里没变」，没有任何报错。
func TestUpdateSettingStoresEveryField(t *testing.T) {
	reg := settingRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	rec := authedPutJSON(t, h, cookie, settingPath, settingBody(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}

	want := registry.PlatformSetting{
		SessionTTL:          8 * time.Hour,
		HTTPReadTimeout:     10 * time.Second,
		HTTPWriteTimeout:    20 * time.Second,
		HTTPShutdownTimeout: 15 * time.Second,
		SecretsBackend:      registry.SecretsBackendDir,
		SecretsDir:          testDirBackendPath,
		GitVerifyTimeout:    10 * time.Second,
		GitVerifyHostKeys:   testHostKeys,
	}
	if reg.setting != want {
		t.Errorf("stored setting =\n%+v\nwant\n%+v", reg.setting, want)
	}
	// 写完之后读回来的还是只有指纹：保存路径不得成为回显 host key 的旁路。
	if strings.Contains(rec.Body.String(), testHostKeys) {
		t.Errorf("the save response echoed the host key material: %s", rec.Body.String())
	}
}

// 保存之后读到的必须是新值。
//
// 这一条守的是「按需读取」那条纪律的边界侧：读取端点若在进程里缓存一份，
// 所有单次调用的行为都不变，差别只在操作者刚保存完的那一次 —— 而那正是
// 轮 2 出过事故的形状（design doc §1.1）。
func TestSettingReadReflectsTheLastWrite(t *testing.T) {
	reg := settingRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	before, _ := bodyOf(t, authedGet(t, h, cookie, settingPath))["data"].(map[string]any)
	if before["gitVerifyTimeoutMs"] != float64(10000) {
		t.Fatalf("gitVerifyTimeoutMs = %v, want the seeded 10000", before["gitVerifyTimeoutMs"])
	}

	rec := authedPutJSON(t, h, cookie, settingPath, settingBody(map[string]any{"gitVerifyTimeoutMs": 25000}))
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 (body %s)", got, rec.Body.String())
	}

	after, _ := bodyOf(t, authedGet(t, h, cookie, settingPath))["data"].(map[string]any)
	if after["gitVerifyTimeoutMs"] != float64(25000) {
		t.Errorf("gitVerifyTimeoutMs = %v after the save, want 25000 — a cached read would still answer the old value",
			after["gitVerifyTimeoutMs"])
	}
}

// 后端与字段必须互相印证，判定由 registry.ValidatePlatformSetting 做，
// 边界层负责把它变成一条调用方读得懂的业务失败。
//
// 最坏的落法是选了 DIR、project 却还留在库里：进程正常起来、校验正常
// 出结论，只是身份来源读成了本地目录，没有任何症状会暴露它。
func TestUpdateSettingRejectsASettingThatContradictsItself(t *testing.T) {
	reg := settingRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	before := reg.setting

	rec := authedPutJSON(t, h, cookie, settingPath, settingBody(map[string]any{
		"secretsProject": "distill-prod",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a bad field combination is a business failure", rec.Code)
	}
	got := bodyOf(t, rec)
	if got["code"] != float64(20001) {
		t.Fatalf("code = %v, want 20001 (body %s)", got["code"], rec.Body.String())
	}
	if msg, _ := got["msg"].(string); !strings.Contains(msg, "secretsProject") {
		t.Errorf("msg = %q, want it to name the field that contradicts the backend", msg)
	}
	if reg.setting != before {
		t.Errorf("setting = %+v, want it untouched by a rejected save", reg.setting)
	}
}

// 时长为零是「会话立即过期、超时保护关掉」，不是「用默认值」。
func TestUpdateSettingRejectsNonPositiveTimeouts(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), settingRegistry())

	for _, field := range []string{
		"sessionTtlSeconds", "httpReadTimeoutMs", "httpWriteTimeoutMs",
		"httpShutdownTimeoutMs", "gitVerifyTimeoutMs",
	} {
		t.Run(field, func(t *testing.T) {
			rec := authedPutJSON(t, h, cookie, settingPath, settingBody(map[string]any{field: 0}))
			if got := bodyOf(t, rec)["code"]; got != float64(20001) {
				t.Errorf("code = %v, want 20001 for a zero %s", got, field)
			}
		})
	}
}

func TestUpdateSettingRejectsMalformedJSON(t *testing.T) {
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), settingRegistry())

	req := httptest.NewRequest(http.MethodPut, settingPath, bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unparseable body is a protocol-level failure", rec.Code)
	}
}

func TestSettingEndpointsRequireSession(t *testing.T) {
	reg := settingRegistry()
	h, _, _ := newTestRouterWithRegistry(t, fixtureReader(), reg)
	before := reg.setting

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		req := httptest.NewRequest(method, settingPath, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", method, rec.Code)
		}
	}
	if reg.setting != before {
		t.Error("an unauthenticated request changed the setting")
	}
}

// registry 内部故障走真实的 500，错误细节一个字都不能进响应体。
//
// 读与写两条分支都要打：只让整个替身失败的话，写路径上的错误处理根本
// 没被执行 —— 而一条断言在没被执行到的分支上永远成立。
func TestSettingEndpointsDoNotLeakRegistryErrorText(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(http.Handler, *http.Cookie) *httptest.ResponseRecorder
		fail func(*memRegistry)
	}{
		{"read", func(h http.Handler, c *http.Cookie) *httptest.ResponseRecorder {
			return authedGet(t, h, c, settingPath)
		}, func(m *memRegistry) { m.failWith = errTestRegistry }},
		{"write", func(h http.Handler, c *http.Cookie) *httptest.ResponseRecorder {
			return authedPutJSON(t, h, c, settingPath, settingBody(nil))
		}, func(m *memRegistry) { m.failWritesWith = errTestRegistry }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := settingRegistry()
			tc.fail(reg)
			h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

			rec := tc.call(h, cookie)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
			}
			if got := bodyOf(t, rec)["msg"]; got != response.CodeInternal.Message() {
				t.Errorf("msg = %q, want the fixed internal-error message", got)
			}
			for _, secret := range []string{"mysql", "10.0.0.5", "3306"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("response leaked %q: %s", secret, rec.Body.String())
				}
			}
		})
	}
}

// 「这次不动信任锚」必须在协议上说得出口。
//
// PUT 是整行替换，而 host key 原文永远读不回来（读取端点只回指纹）。
// 于是在这个字段上，「不修改」与「清空」原本长得一模一样：一个手上不再
// 留着 known_hosts 原文的操作者，会连会话 TTL 都改不了 —— 表单要么拦下
// 每一次保存，要么替他把信任锚抹掉。缺席即保持，这个死结才解开
// （final review I3）。
//
// 三个方向一起断言：TTL 真的改了、信任锚原样留着、响应里的指纹仍然是
// 当前那一份 —— 少了最后一条，一个把信任锚清掉却照常回 200 的实现也能
// 通过前两条里的任何一条。
func TestUpdateSettingKeepsTheTrustAnchorWhenTheFieldIsAbsent(t *testing.T) {
	reg := settingRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)

	body := settingBody(nil)
	// 操作者只想改会话 TTL，手上没有 known_hosts 原文，也没打算碰信任锚。
	delete(body, "gitVerifyHostKeys")
	body["sessionTtlSeconds"] = 3600

	rec := authedPutJSON(t, h, cookie, settingPath, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(0) {
		t.Fatalf("code = %v, want 0 — leaving the trust anchor alone must not block the save (body %s)",
			got, rec.Body.String())
	}
	if reg.setting.SessionTTL != time.Hour {
		t.Errorf("sessionTtl = %v, want 1h — the operator could not change a setting he is allowed to change",
			reg.setting.SessionTTL)
	}
	if reg.setting.GitVerifyHostKeys != testHostKeys {
		t.Fatalf("gitVerifyHostKeys = %q, want it untouched — an absent field silently removed the trust anchor",
			reg.setting.GitVerifyHostKeys)
	}
	data, _ := bodyOf(t, rec)["data"].(map[string]any)
	if fp, _ := data["gitVerifyHostKeysFingerprint"].(string); fp == "" {
		t.Error("the response reports no trust anchor after a save that never touched it")
	}
}

// 显式清空必须被服务端拒绝，而不是被浏览器拦下。
//
// 设置页确实拦过这件事，但那是一份镜像、不是判定（规范 §34：前端不是
// 安全边界）—— 一次 curl、一个将来新增的页面，都能一句话抹掉信任锚。
// 清空的后果不是「退化成不校验」（gitverify.New 拒绝构造，失败朝关），
// 而是一次无声的能力丧失：此后每一次 Git 校验都出不了结论，没有人做过
// 这个决定，而原文再也读不回来，无法撤销。
func TestUpdateSettingRefusesToClearTheTrustAnchor(t *testing.T) {
	reg := settingRegistry()
	h, _, cookie := newTestRouterWithRegistry(t, fixtureReader(), reg)
	before := reg.setting

	rec := authedPutJSON(t, h, cookie, settingPath,
		settingBody(map[string]any{"gitVerifyHostKeys": ""}))
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Fatalf("code = %v, want 20001 — a single PUT cleared the platform's SSH trust anchor (body %s)",
			got, rec.Body.String())
	}
	if msg, _ := bodyOf(t, rec)["msg"].(string); !strings.Contains(msg, "gitVerifyHostKeys") {
		t.Errorf("msg = %q, want it to name the field that was refused", msg)
	}
	if reg.setting != before {
		t.Errorf("setting = %+v, want it untouched by a rejected save", reg.setting)
	}
}
