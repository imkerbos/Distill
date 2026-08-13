// Command distill-api 提供 Distill 平台的 HTTP 接口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/buildinfo"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/gitverify"
	"github.com/imkerbos/Distill/internal/httpapi"
	applog "github.com/imkerbos/Distill/internal/log"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/secrets"
	"github.com/imkerbos/Distill/internal/secrets/gcpsecrets"
	"github.com/imkerbos/Distill/internal/settings"
	"github.com/imkerbos/Distill/internal/store"
)

func main() {
	configPath := flag.String("config", "configs/demo.yaml", "path to the config file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		// 日志器可能尚未构造成功，此处直接写 stderr。
		_, _ = os.Stderr.WriteString("startup failed: " + err.Error() + "\n")
		os.Exit(1)
	}
}

// run 装配并启动服务，返回启动或退出过程中的致命错误。
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger, err := applog.New(cfg.Log.Level, os.Stdout)
	if err != nil {
		return err
	}
	logger.Info("starting", "version", buildinfo.Version(), "addr", cfg.Server.Addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := mysqlregistry.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// 迁移在启动时执行：schema 落后于代码的进程不该接受请求，
	// 而在启动时失败比在第一个请求时失败更容易定位。
	//
	// 也是设置能被读到的前提：种子行由 000005 迁移写下，表不能为空。
	if err := mysqlregistry.Migrate(cfg.Database, "migrations"); err != nil {
		return err
	}

	reg := mysqlregistry.New(db)
	// reader 拿到的是注册表本身而不是启动时读出的一份集群清单：
	// 集群是否受平台管理必须在每个请求上现查（spec §4.5）。传快照的话，
	// 下线一个集群之后 /security 与 /policy-preview 会继续供数直到进程
	// 重启 —— 操作者收到「已下线」的确认，事实却相反。
	reader := store.NewFixtureReader(fixture.Load(), reg)

	// 同一个道理，设置也传提供者而不是取出来的一份值：Git 校验相关的
	// 全部配置都从设置页改，改完必须立即生效（design doc §1.1）。
	settingsProvider := settings.New(reg)

	// 例外只有这几项，且是被消费方的形态决定的：http.Server 与
	// auth.SessionStore 在构造的那一刻就把超时收进了自己内部，之后没有
	// 任何接口能改它们。改这几项仍需重启，这是已知的空缺，不是可以顺手
	// 扩大到其余设置的先例 —— 其余一切走 settingsProvider 现读。
	bootSetting, err := settingsProvider.Current(ctx)
	if err != nil {
		return err
	}

	r := chi.NewRouter()
	r.Mount("/healthz", newHealthHandler())
	r.Mount("/", httpapi.NewRouter(httpapi.Deps{
		Sessions: auth.NewSessionStore(bootSetting.SessionTTL, nil),
		// 引导账号是文件里唯一的账号，也是首次登录的唯一入口
		// （design doc §1.2）。
		Verifier:    auth.NewVerifier([]config.User{cfg.Auth.BootstrapUser}),
		Logger:      logger,
		Reader:      reader,
		Registry:    reg,
		GitVerifier: newSettingsGitVerifier(settingsProvider, logger),
		// demo 的默认时间窗取 fixture 数据的实际范围。任何"最近 N 天"
		// 的取值都会随真实时间推移而在某天返回 0 条 —— demo 会在没有
		// 人改动代码的情况下自己坏掉。接真实存储时这里换成有界窗口。
		DefaultWindow: reader.DataWindow(),
	}))

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      r,
		ReadTimeout:  bootSetting.HTTPReadTimeout,
		WriteTimeout: bootSetting.HTTPWriteTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), bootSetting.HTTPShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// settingsGitVerifier 在**每一次校验**上按当前设置现装一个校验器。
//
// 不在启动时装一次：凭据后端、Git 超时与 SSH host key 都从设置页改，
// 装一次就等于「保存成功、毫无变化，直到重启」—— 而把这些挪进数据库
// 正是为了终结这件事（design doc §1.1）。轮 2 已经在启动快照上出过一次
// 事故，而这里的后果更重：信任锚换了却不生效。
//
// 每次校验多一次单行主键命中与一次解析器构造。校验本身是一次带秒级
// 超时的出站 clone，这点开销在它面前可忽略（design doc §8）。
type settingsGitVerifier struct {
	settings *settings.Provider
	logger   *slog.Logger
}

// newSettingsGitVerifier 构造一个按当前设置现装校验器的 GitVerifier。
func newSettingsGitVerifier(p *settings.Provider, logger *slog.Logger) *settingsGitVerifier {
	return &settingsGitVerifier{settings: p, logger: logger}
}

// VerifyRepo 用当前设置做一次仓库级只读校验。
//
// **装不出校验器时返回 NOT_VERIFIED，绝不是 OK。** 读不到设置、后端选了
// NONE、host key 为空、超时非正 —— 每一种都是「这次检查没做成」，而没做过
// 的检查不是通过了的检查（spec §3.2）。退化成「没有校验器所以放行」会让
// 一次设置失误变成一串看不出问题的 OK，而这条链路的终点是生产集群的
// 策略集合。
//
// 时刻同样留 nil：没有发生过校验，就没有那个时刻，而一个带时间戳的
// NOT_VERIFIED 会在界面上显示成一次从未发生的校验。
func (v *settingsGitVerifier) VerifyRepo(
	ctx context.Context, r registry.GitRepo,
) (registry.RepoVerifyResult, *time.Time) {
	gv, ok := v.current(ctx)
	if !ok {
		return registry.RepoVerifyNotVerified, nil
	}
	return gv.VerifyRepo(ctx, r)
}

// VerifyPath 用当前设置做一次路径级只读校验。
//
// repoResult 原样交给底层：路径级以仓库级为前提，而「先取仓库级结论」
// 是调用方的责任（design doc §3.3），这一层不替它决定。
//
// 装不出校验器时同样是 NOT_VERIFIED，理由见 VerifyRepo。这两个方法各自
// 兜一次，不是重复：少掉任何一处，那一层就会在设置失误时退化成放行。
func (v *settingsGitVerifier) VerifyPath(
	ctx context.Context, r registry.GitRepo,
	repoResult registry.RepoVerifyResult, policyPath string,
) (registry.BindingVerifyResult, *time.Time) {
	gv, ok := v.current(ctx)
	if !ok {
		return registry.BindingVerifyNotVerified, nil
	}
	return gv.VerifyPath(ctx, r, repoResult, policyPath)
}

// current 按当前设置现装一个校验器；装不出来时第二个返回值为 false。
//
// 两个方法共用它，而不是各写一遍读设置与装配：分成两份之后，「设置改完
// 立即生效」这条只会在其中一份上被守住，而另一份会在某次改动里悄悄退回
// 启动快照。
func (v *settingsGitVerifier) current(ctx context.Context) (httpapi.GitVerifier, bool) {
	s, err := v.settings.Current(ctx)
	if err != nil {
		v.logger.Error("cannot read the platform setting for git verification", "error", err)
		return nil, false
	}
	gv, err := newGitVerifier(ctx, s)
	if err != nil {
		v.logger.Error("cannot build a git verifier from the current setting", "error", err)
		return nil, false
	}
	if gv == nil {
		// 后端是 NONE：操作者明确选了「不解析凭据」，于是也就没有校验。
		return nil, false
	}
	return gv, true
}

// newGitVerifier 按一份设置装配 Git 绑定校验器。
//
// 后端为 NONE 时返回一个**真正的 nil 接口**，而不是包着 nil 指针的接口值：
// 调用方用 `gv == nil` 判断"没有校验这回事"，返回
// (*gitverify.Verifier)(nil) 会让那个判断为假，随后每次校验都在一个空指针
// 上调方法。返回类型写成接口而不是具体类型，正是为了这一点。
//
// timeout 与 host keys 显式传给 gitverify.New：它拒绝非正超时、也拒绝
// 空 host key 集合，两者都不给默认值 —— 缺失必须在这里变成一个错误，
// 而不是一个「装出来了但每次校验都失败」的校验器。设置在保存时已经过
// registry.ValidatePlatformSetting，settings.Provider 读出来时又判了一次；
// 这里的失败是最后一道，不是唯一一道。
func newGitVerifier(ctx context.Context, s registry.PlatformSetting) (httpapi.GitVerifier, error) {
	resolver, err := newSecretResolver(ctx, s)
	if err != nil {
		return nil, err
	}
	if resolver == nil {
		return nil, nil
	}
	v, err := gitverify.New(resolver, []byte(s.GitVerifyHostKeys), s.GitVerifyTimeout)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// newSecretResolver 按设置选出的后端装配凭据解析器。
//
// 后端是操作者在设置页做出的一个明确选择，不再从「填了哪几个字段」反推
// （design doc §2）。字段与后端是否互相印证由
// registry.ValidatePlatformSetting 保证，这里只做装配 —— 「设置说了什么」
// 与「据此构造什么」分开，两边各自可测，谁被改坏了都说得出是哪一边。
//
// 每个分支都显式写出返回值，不用 `return gcpsecrets.NewResolver(...)` 那种
// 直接转发：构造失败时它返回的是 (*gcpsecrets.Resolver)(nil)，装进接口就成了一个
// 非 nil 的接口值，而调用方正是用 `resolver == nil` 判断「没有解析器」的。
func newSecretResolver(ctx context.Context, s registry.PlatformSetting) (secrets.Resolver, error) {
	switch s.SecretsBackend {
	case registry.SecretsBackendDir:
		return secrets.NewDirResolver(s.SecretsDir), nil
	case registry.SecretsBackendSecretManager:
		r, err := gcpsecrets.NewResolver(ctx, s.SecretsProject, s.SecretsPrefix)
		if err != nil {
			return nil, err
		}
		return r, nil
	case registry.SecretsBackendNone:
		return nil, nil
	default:
		// SecretsBackend 是封闭枚举；走到这里说明加了取值却没加装配分支。
		// 静默返回一个「没有解析器」会把它变成一次悄悄关掉校验的上线。
		return nil, fmt.Errorf("unknown secrets backend %q", s.SecretsBackend)
	}
}

// newHealthHandler 返回存活探针处理器。
//
// 探针也走统一包络：让前端与运维只需要理解一种响应形状。
func newHealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteOK(w, map[string]string{"version": buildinfo.Version()})
	})
}
