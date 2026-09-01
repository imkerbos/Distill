package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/imkerbos/Distill/internal/httpapi"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/secrets"
	"github.com/imkerbos/Distill/internal/secrets/dbsecrets"
	"github.com/imkerbos/Distill/internal/secrets/gcpsecrets"
	"github.com/imkerbos/Distill/internal/settings"
)

// sampleHostKey 是一条 known_hosts 记录。公钥不是机密，host key 本就与
// 凭据分开存放（spec §2.2）。
const sampleHostKey = "gitlab.example.com ssh-ed25519 " +
	"AAAAC3NzaC1lZDI1NTE5AAAAIKyjWioKIYrbPTzY9F8JKIElwSThZ4xuqtqQPGo9tDIg"

// baseSetting 返回一份不解析凭据、因而也不做校验的设置。
func baseSetting() registry.PlatformSetting {
	return registry.PlatformSetting{
		SessionTTL:          8 * time.Hour,
		HTTPReadTimeout:     10 * time.Second,
		HTTPWriteTimeout:    20 * time.Second,
		HTTPShutdownTimeout: 15 * time.Second,
		SecretsBackend:      registry.SecretsBackendNone,
		GitVerifyTimeout:    10 * time.Second,
	}
}

// dirSetting 返回一份用目录解析器、host key 与超时都齐备的设置。
func dirSetting(dir string) registry.PlatformSetting {
	s := baseSetting()
	s.SecretsBackend = registry.SecretsBackendDir
	s.SecretsDir = dir
	s.GitVerifyHostKeys = sampleHostKey
	return s
}

// mutableSource 是 settings.Source 的替身，可在两次读取之间换一份设置。
type mutableSource struct {
	current registry.PlatformSetting
	err     error
}

func (m *mutableSource) Setting(context.Context) (registry.PlatformSetting, error) {
	if m.err != nil {
		return registry.PlatformSetting{}, m.err
	}
	return m.current, nil
}

// quietLogger 是一个丢弃输出的日志器，避免测试输出被装配失败的日志淹没。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// sampleRepo 是一个形态合法的策略仓库。
//
// credentialRef 指向一个不存在的文件，于是 DirResolver 在第一步就失败，
// 校验返回 CREDENTIAL_UNRESOLVED —— **一次出站都不会发生**，这条测试
// 因此不依赖网络（见 gitverify.Verifier.cloneBranch 的调用顺序）。
func sampleRepo() registry.GitRepo {
	return registry.GitRepo{
		ID:            "policies",
		URL:           "ssh://git@gitlab.example.com/distill/policies.git",
		Branch:        "main",
		CredentialRef: "missing-credential",
	}
}

// 装配出来的校验器必须满足边界层要的那个接口。
//
// 编译期断言而不是靠某条测试碰巧调到两个方法：httpapi.GitVerifier 是两层
// 校验各一个方法，少实现一个的表现是装配处编译不过 —— 这一行让那次失败
// 落在这里，而不是落在一次改了 Deps 装配的提交里。
var _ httpapi.GitVerifier = (*settingsGitVerifier)(nil)

// 校验器必须在**每一次使用**上按当前设置现装，不是启动时装一次。
//
// 这是本任务存在的理由。快照形态下，操作者在设置页选了凭据后端、保存、
// 看到成功，然后每一次校验仍然走着进程启动那一刻的那份设置 —— 直到重启。
// 轮 2 已经在启动快照上出过一次事故（下线的集群继续被服务）。
//
// 两个方向都断言：NONE → DIR 与 DIR → NONE。只断一个方向的话，一个把
// 结论写死成某个值的实现会有一半是绿的。
func TestGitVerifierIsBuiltFromTheCurrentSettingOnEveryUse(t *testing.T) {
	src := &mutableSource{current: baseSetting()}
	v := newSettingsGitVerifier(settings.New(src), quietLogger(), nil, nil)
	r := sampleRepo()

	if got, _ := v.VerifyRepo(t.Context(), r); got != registry.RepoVerifyNotVerified {
		t.Fatalf("verdict = %q with the NONE backend, want NOT_VERIFIED", got)
	}

	// 操作者在设置页选了目录后端并填了 host key。
	src.current = dirSetting(t.TempDir())

	got, at := v.VerifyRepo(t.Context(), r)
	if got == registry.RepoVerifyNotVerified {
		t.Errorf("verdict = %q after the backend changed to DIR, want the new setting to be in effect "+
			"— a verifier built once at startup keeps answering NOT_VERIFIED until the process restarts", got)
	}
	if got != registry.RepoVerifyCredentialUnresolved {
		t.Errorf("verdict = %q, want CREDENTIAL_UNRESOLVED from the directory resolver", got)
	}
	// 校验确实发生过，因此有一个时刻。少了这条，一个「什么都没做就回一个
	// 结论」的实现在上面两条断言下也可能是绿的。
	if at == nil {
		t.Error("verifiedAt is nil, want the moment this check happened")
	}

	// 反方向：改回 NONE 之后，启动时装好的那个校验器不得继续生效。
	src.current = baseSetting()

	if got, _ := v.VerifyRepo(t.Context(), r); got != registry.RepoVerifyNotVerified {
		t.Errorf("verdict = %q after the backend changed back to NONE, want NOT_VERIFIED "+
			"— a cached verifier would keep resolving credentials that the operator turned off", got)
	}
}

// 读不到设置不得退化成「没有校验器所以放行」。
//
// 得不出结论的检查是 NOT_VERIFIED，永远不是 OK。
//
// 两层各打一次：两个方法各自读一次设置、各自装一次校验器，只测其中一个的
// 话，另一个漏掉这道兜底也不会有测试变红 —— 而漏掉的那一层会在设置读不到
// 时把结论朝「可信」的方向开。
func TestGitVerifierIsNotVerifiedWhenTheSettingCannotBeRead(t *testing.T) {
	src := &mutableSource{err: errBrokenSource}
	v := newSettingsGitVerifier(settings.New(src), quietLogger(), nil, nil)

	repoResult, at := v.VerifyRepo(t.Context(), sampleRepo())
	if repoResult != registry.RepoVerifyNotVerified {
		t.Fatalf("repo verdict = %q when the setting cannot be read, want NOT_VERIFIED", repoResult)
	}
	if at != nil {
		t.Errorf("repo verifiedAt = %v, want nil — no check happened, so there is no such moment", at)
	}

	// 路径级传 OK 进去，正是为了让「仓库级不是 OK 就短路」那条规则挡不住
	// 这次调用 —— 挡住的话，这条断言就永远成立，什么都证明不了。
	pathResult, pathAt := v.VerifyPath(t.Context(), sampleRepo(), registry.RepoVerifyOK, "clusters/prod")
	if pathResult != registry.BindingVerifyNotVerified {
		t.Fatalf("path verdict = %q when the setting cannot be read, want NOT_VERIFIED", pathResult)
	}
	if pathAt != nil {
		t.Errorf("path verifiedAt = %v, want nil — no check happened", pathAt)
	}
}

// 设置能读到但装不出校验器（缺 host key）时同样是 NOT_VERIFIED。
//
// 与上一条分开：一个只在读失败时兜底、装配失败却 panic 或返回 OK 的实现，
// 在上一条下是绿的。两层同样各打一次，理由同上。
func TestGitVerifierIsNotVerifiedWhenItCannotBeBuilt(t *testing.T) {
	s := dirSetting(t.TempDir())
	s.GitVerifyHostKeys = "" // 没有 host key 就没有 SSH 校验，gitverify.New 拒绝构造。
	v := newSettingsGitVerifier(settings.New(&mutableSource{current: s}), quietLogger(), nil, nil)

	if got, _ := v.VerifyRepo(t.Context(), sampleRepo()); got != registry.RepoVerifyNotVerified {
		t.Fatalf("repo verdict = %q with no host keys, want NOT_VERIFIED — never OK", got)
	}
	got, _ := v.VerifyPath(t.Context(), sampleRepo(), registry.RepoVerifyOK, "clusters/prod")
	if got != registry.BindingVerifyNotVerified {
		t.Fatalf("path verdict = %q with no host keys, want NOT_VERIFIED — never OK", got)
	}
}

// errBrokenSource 是设置读取失败的替身错误。
var errBrokenSource = errors.New("settings source is unavailable")

// 后端为 NONE 时必须返回一个**真正的 nil 接口**。
//
// 返回 (*gitverify.Verifier)(nil) 一样能通过编译，但调用方用 `gv == nil`
// 判断"没有校验这回事"，包着 nil 指针的接口值会让那个判断为假 —— 随后
// 每次校验都在空指针上调方法。
func TestNewGitVerifierIsNilForTheNoneBackend(t *testing.T) {
	v, err := newGitVerifier(t.Context(), baseSetting(), nil, nil)
	if err != nil {
		t.Fatalf("newGitVerifier() error = %v", err)
	}
	if v != nil {
		t.Errorf("verifier = %#v, want an untyped nil interface", v)
	}
}

func TestNewGitVerifierBuildsAVerifierWhenConfigured(t *testing.T) {
	v, err := newGitVerifier(t.Context(), dirSetting(t.TempDir()), nil, nil)
	if err != nil {
		t.Fatalf("newGitVerifier() error = %v", err)
	}
	if v == nil {
		t.Error("verifier = nil, want a configured verifier")
	}
}

// 超时必须由装配方显式传下去。
//
// gitverify.New 拒绝非正超时而不是取一个默认值；如果这里漏传（或传了
// 零值），装配必须失败。少了这条，一次装配疏忽的表现是"每次校验都用一个
// 谁也说不出来的超时"，而那正是本项目最难在事后复盘的一类问题。
func TestNewGitVerifierRefusesAMissingTimeout(t *testing.T) {
	s := dirSetting(t.TempDir())
	s.GitVerifyTimeout = 0

	if _, err := newGitVerifier(t.Context(), s, nil, nil); err == nil {
		t.Fatal("newGitVerifier() = nil error, want a failure for a missing timeout")
	}
}

// 没有 host key 就没有 SSH 校验，而回退到不校验等于接受任意中间人。
func TestNewGitVerifierRefusesMissingHostKeys(t *testing.T) {
	s := dirSetting(t.TempDir())
	s.GitVerifyHostKeys = ""

	if _, err := newGitVerifier(t.Context(), s, nil, nil); err == nil {
		t.Fatal("newGitVerifier() = nil error, want a failure for missing host keys")
	}
}

// 后端为 NONE 时装不出解析器，且必须是一个真正的 nil 接口。
func TestNewSecretResolverIsNilForTheNoneBackend(t *testing.T) {
	r, err := newSecretResolver(t.Context(), baseSetting(), nil, nil)
	if err != nil {
		t.Fatalf("newSecretResolver() error = %v", err)
	}
	if r != nil {
		t.Errorf("resolver = %#v, want an untyped nil interface", r)
	}
}

// 选了 DIR 就装目录解析器。
func TestNewSecretResolverPicksTheDirectoryBackend(t *testing.T) {
	r, err := newSecretResolver(t.Context(), dirSetting(t.TempDir()), nil, nil)
	if err != nil {
		t.Fatalf("newSecretResolver() error = %v", err)
	}
	if _, ok := r.(*secrets.DirResolver); !ok {
		t.Fatalf("resolver = %T, want *secrets.DirResolver", r)
	}
}

// 选了 SECRET_MANAGER 就**不能**装目录解析器。
//
// 这条是唯一能证明「生产设置不会落到本地目录」的断言，所以它断的是类型
// 而不是「非 nil」：一个把选择写死成 NewDirResolver 的实现，在「非 nil」
// 下是绿的，在这里是红的。
//
// 本机没有 GCP 凭据，gcpsecrets.NewResolver 是成是败取决于环境，因此两种结果都
// 接受 —— 但只接受这两种：要么带着构造错误失败，要么给出 *gcpsecrets.Resolver。
// 落到 *DirResolver 一律算失败。真实的 API 调用不在这条断言的覆盖范围内。
func TestNewSecretResolverNeverFallsBackToTheDirectoryBackend(t *testing.T) {
	s := baseSetting()
	s.SecretsBackend = registry.SecretsBackendSecretManager
	s.SecretsProject = "distill-prod"
	s.SecretsPrefix = "distill-git-"

	r, err := newSecretResolver(t.Context(), s, nil, nil)
	if _, ok := r.(*secrets.DirResolver); ok {
		t.Fatalf("resolver = %T for a Secret Manager setting, want the local directory backend never to be reached", r)
	}
	if err != nil {
		t.Logf("newSecretResolver() error = %v (no application default credentials on this machine)", err)
		return
	}
	if _, ok := r.(*gcpsecrets.Resolver); !ok {
		t.Fatalf("resolver = %T, want *gcpsecrets.Resolver", r)
	}
}

// 未登记的后端取值必须是一个错误，不是一次悄悄关掉校验的上线。
func TestNewSecretResolverRejectsAnUnregisteredBackend(t *testing.T) {
	s := baseSetting()
	s.SecretsBackend = registry.SecretsBackend("LOOKS_FINE")

	if _, err := newSecretResolver(t.Context(), s, nil, nil); err == nil {
		t.Fatal("newSecretResolver() = nil error for an unregistered backend, want a failure")
	}
}

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	newHealthHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got["code"] != float64(0) {
		t.Errorf("code = %v, want 0", got["code"])
	}
}

// **有密钥就装出保管处，没密钥就没有。**
//
// 这条用例补的是一次真实的遗漏：处理器与存储层都写好、都有用例，
// 而 Deps.CredentialStore 从来没有被赋值。两边各自的用例全绿，
// 界面上的表现是"这个部署没有把凭据交给平台保管"——一句正确的话，
// 描述的却是一个不该出现的状态（2026-09-01 UAT 实测）。
//
// 装配层的遗漏测不出来，是因为每一半都被单独测过。
func TestCredentialStoreIsAssembledWhenAKeyIsPresent(t *testing.T) {
	// sql.Open 不连接，只造一个句柄——这一条要证的是"装配走到了构造"，
	// 不是"能连上库"。
	db, err := sql.Open("mysql", "user:pass@tcp(127.0.0.1:1)/none")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	kek := bytes.Repeat([]byte{7}, dbsecrets.KEKSize)
	if got := newCredentialStore(db, kek, quietLogger()); got == nil {
		t.Fatal("有密钥、有数据库句柄，却没装出凭据保管处 —— " +
			"界面上会说这个部署不保管凭据，而那是一句描述了错误状态的正确的话")
	}
}

// 没有密钥就不装：一个装着空密钥的实现会在每次写入时失败，
// 而失败信息说不出真正的原因是没配密钥。
func TestCredentialStoreIsAbsentWithoutAKey(t *testing.T) {
	db, err := sql.Open("mysql", "user:pass@tcp(127.0.0.1:1)/none")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if got := newCredentialStore(db, nil, quietLogger()); got != nil {
		t.Error("没有密钥却装出了凭据保管处")
	}
}

// **Deps 上那一行赋值必须在。**
//
// 上面两条用例测的是 newCredentialStore 这个函数，而 2026-09-01 漏掉的是
// 装配里的一行赋值：函数写好了、有用例、全绿，Deps.CredentialStore 却
// 从来没被赋过值。删掉那一行，上面两条依然全过。
//
// 这一条按源码断言，因为装配发生在 run() 里、要连数据库才走得到，
// 而"这一行在不在"不需要连数据库就能回答。源码断言脆（改个变量名就红），
// 但它的失败方式是显眼的，而它防的那个缺陷的失败方式是"界面上说这个部署
// 不保管凭据"——一句正确的话，描述的却是一个不该出现的状态。
func TestDepsCarriesTheCredentialStore(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !bytes.Contains(src, []byte("CredentialStore: newCredentialStore(")) {
		t.Error("httpapi.Deps 上没有装配 CredentialStore —— " +
			"凭据保管处存在、有用例、但接口层永远拿不到它")
	}
}
