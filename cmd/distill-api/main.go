// Command distill-api 提供 Distill 平台的 HTTP 接口。
package main

import (
	"context"
	"crypto/tls"
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
	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/fleet"
	"github.com/imkerbos/Distill/internal/gitverify"
	"github.com/imkerbos/Distill/internal/gitwrite"
	"github.com/imkerbos/Distill/internal/httpapi"
	applog "github.com/imkerbos/Distill/internal/log"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/secrets"
	"github.com/imkerbos/Distill/internal/secrets/gcpsecrets"
	"github.com/imkerbos/Distill/internal/settings"
	"github.com/imkerbos/Distill/internal/snapshotstore"
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
	//
	// **这里挂的是分派器，不是某一个 Reader**（design doc 2026-08-18）。它在
	// 每一次调用上按集群**登记的**来源选出作答的 Reader：FIXTURE 走合成数据集，
	// COLLECTED 走 collectstore。同一个理由 —— 一个集群的来源在库里改掉之后，
	// 进程不该继续按旧的答，直到重启。
	//
	// 接线之前那一层「页面不会显示错误的生产结论」的保障是不需要代码正确性的：
	// 装配层只装 FixtureReader，一个真集群的数字到不了屏幕上。**接线之后那层
	// 没了**（design doc §1）—— 从这一行起，操作者看到的数字是否描述真实集群，
	// 完全依赖分派、六个读方法、身份解析、完整度传导与前端渲染的正确性。
	//
	// 回退手段只有一个：回滚这次部署。`data_source` 没有写路径，也不该有 ——
	// 「把某个集群临时切回演示数据」不是一个安全位置，那是一句关于生产集群的
	// 假话（design doc §5）。
	//
	// 默认时间窗也从这里出（design doc 2026-08-18 §3.1）。此前它在这一行下面
	// 被算成一个常量（`newFixtureReader(reg).DataWindow()`）并注入
	// httpapi.Deps.DefaultWindow —— 合成数据集自己的那段约 36 秒。分派器现在
	// 按集群现答：FIXTURE 仍是那段固定范围，COLLECTED 是它最近一次摄入窗口，
	// 没摄入过则答不出、于是页面说「还没有可用的采集数据」。
	reader := newReader(db, reg)

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
		// 引导账号是文件里唯一的账号，也是首次登录的入口；账号表跟着一起
		// 传进去，因为角色要在每次判定时从账号记录现读，而引导账号本身
		// 也只在库里没有启用中的管理员时才可用
		// （design doc 2026-08-14 §2、§4）。
		Verifier:    auth.NewVerifier(cfg.Auth.BootstrapUser, reg),
		Logger:      logger,
		Reader:      reader,
		Registry:    reg,
		GitVerifier: newSettingsGitVerifier(settingsProvider, logger),
		// 写回的两条持久化路径与写入器。同一个 *mysqlregistry.Store 同时
		// 满足 registry.Store 与 registry.WritebackStore —— 收窄成两个接口
		// 是为了让边界层只拿到它真正需要的那几个方法，不是为了换实现。
		Writeback:    reg,
		PolicyWriter: newSettingsPolicyWriter(settingsProvider, logger),
		// 资产采集的只读摘要。拉取式采集的写入侧属于 cmd/distill-collector
		// —— 这里只拿读的那一面，主服务不持有任何 Kubernetes 凭据。
		Collection: snapshotstore.New(db),
		// 流量摄入的只读摘要，与资产采集并列。
		FlowIngest: snapshotstore.New(db),
		// 对账历史与摄入摘要同一个库：拆成两个参数只会给装配方一个传进
		// 两个不同数据库的位置（同 collectstore.New 那条）。
		Reconciliations: snapshotstore.New(db),
		// 推送式接入的落库口（design doc 2026-08-18）。
		//
		// 这里出现一条写路径，与上面那句「只拿读的那一面」并不矛盾：写进来
		// 的是**被管集群自己推上来的观测**，主服务依然没有任何 Kubernetes
		// 凭据，也依然不会去连那个集群。方向反了 —— 这正是推送式接入的
		// 全部意义。
		AgentSink: snapshotstore.New(db),
		// 推送落库之后的身份推导。没有它，推送式接入的集群
		// pod_identity_interval 是空的，而六个读方法每一个都从那张表出发
		// —— 页面上显示「这个集群还没有可用的采集数据」，而资产其实已经
		// 在库里了（design doc 2026-08-18）。
		AgentDeriver: snapshotstore.New(db),
		// 推送式接入的流量落库口（design doc 2026-08-18-agent-flow-ingest）。
		//
		// 与 AgentSink 分开装：资产与流量是两条独立的链路，一个集群可以只推
		// 资产（还没接上流量来源）。两条都指向同一个 Store 不等于它们该共用
		// 一个字段 —— 装配上分得开，才谈得上"这个部署收不下流量"这种形态。
		AgentFlowSink: snapshotstore.New(db),
		// 归属判定所需的 fleet 登记，**每次请求现读**。
		//
		// 装配时抄一份等于把判定钉在进程启动那一刻：那之后接入的集群，
		// 它网段里的每一个地址都会被判成 EXTERNAL —— 一个答得出、又不报错
		// 的错误结论（design doc §3.4）。
		Fleet: func(ctx context.Context) (*cluster.Registry, error) {
			clusters, err := reg.Clusters(ctx)
			if err != nil {
				return nil, err
			}
			// 网段登记坏掉的集群照样进判定表，只是没有网段：让它消失会让
			// Classify 把它的 Pod IP 判成 EXTERNAL 或另一个集群的，而带着
			// 空网段留下来只会让判定退化成 UNKNOWN 并附上缺口原因。
			// 失败方向朝「答不出」，不朝「自信地答错」。
			out, unusable := fleet.FromRegistry(clusters)
			for _, id := range unusable {
				logger.Warn("a cluster has an unusable CIDR registration; ingest classification will degrade to UNKNOWN",
					"cluster", id)
			}
			return out, nil
		},
	}))

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      r,
		ReadTimeout:  bootSetting.HTTPReadTimeout,
		WriteTimeout: bootSetting.HTTPWriteTimeout,
	}
	serveTLS := cfg.Server.TLSCertFile != ""
	if serveTLS {
		// 下限钉在 1.2：agent 那一侧的客户端已经要求 1.2（sink.go），
		// 服务端不设下限的话，协商结果由对端决定 —— 而 token 正是从那条
		// 连接上过去的。
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	logger.Info("http server listening", "addr", cfg.Server.Addr, "tls", serveTLS)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if serveTLS {
			err = srv.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// Drift 用当前设置做一次只读的漂移检测。
//
// 装不出校验器时答 UNKNOWN，**不是 IN_SYNC**：未配置 secrets 的部署根本
// 没去看过仓库，而"一致"会让操作者以为下发的东西还在
// （design doc 2026-08-18-drift-detection §3）。
func (v *settingsGitVerifier) Drift(
	ctx context.Context, r registry.GitRepo, policyPath, lastWrittenCommit string,
) registry.DriftResult {
	gv, ok := v.current(ctx)
	if !ok {
		return registry.DriftUnknown
	}
	return gv.Drift(ctx, r, policyPath, lastWrittenCommit)
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

// ErrPolicyWriterUnavailable 表示按当前设置装不出一个写入器。
//
// 单列一个哨兵而不是把底层错误往上传：它会一路走到 httpapi 的结论映射，
// 而那里的 default 分支既不回传也不记录错误文本 —— 装配失败的原因（读不到
// 设置、host key 解析不了、超时非正）留在本层的日志里，那是唯一该看见它的
// 地方（规范 §19、§22）。
//
// 导出而不是包内私有：httpapi.PolicyWriter 只承诺返回 error，调用方要区分
// 「这次没能装出写入器」与「远端拒绝了这次推送」时，得有一个认得出的目标。
var ErrPolicyWriterUnavailable = errors.New("policy writer unavailable for the current setting")

// settingsPolicyWriter 在**每一次推送**上按当前设置现装一个写入器。
//
// 与 settingsGitVerifier 完全同形，理由也一样：凭据后端、Git 超时与 SSH
// host key 都从设置页改，装一次就等于「保存成功、毫无变化，直到重启」
// （design doc 2026-08-13 §1.1）。写路径上这件事更重 —— 信任锚换了却不
// 生效，而这条链路行使的是 deploy key 的**写**权限。
//
// 每次推送多一次单行主键命中与一次解析器构造。推送本身是一次浅克隆加一次
// 出站推送，这点开销在它面前可忽略。出计划时的那次只读枚举同理。
type settingsPolicyWriter struct {
	settings *settings.Provider
	logger   *slog.Logger
}

// newSettingsPolicyWriter 构造一个按当前设置现装写入器的 PolicyWriter。
func newSettingsPolicyWriter(p *settings.Provider, logger *slog.Logger) *settingsPolicyWriter {
	return &settingsPolicyWriter{settings: p, logger: logger}
}

// Push 用当前设置装一个写入器并把计划推出去。
//
// **装不出来时返回错误，绝不静默地当作成功。** 读不到设置、后端选了 NONE、
// host key 为空、超时非正 —— 每一种都是「这次推不了」，而返回一个空 SHA 加
// nil 会让调用方把它当成推成了，随后写下一个不存在的 last_written_commit
// （规范 §49，失败方向朝关）。
func (v *settingsPolicyWriter) Push(
	ctx context.Context, repo registry.GitRepo, plan registry.WritebackPlan,
) (string, error) {
	writer, err := v.writer(ctx)
	if err != nil {
		return "", err
	}
	return writer.Push(ctx, repo, plan)
}

// List 用当前设置装一个写入器，只读地枚举一次策略仓库。
//
// 与 Push 共用同一段装配，因此也共用同一条受守卫的出站链路：**这里只读**，
// 装出来的是同一个对象不代表这次调用会写（gitwrite.Writer.List 的契约）。
func (v *settingsPolicyWriter) List(
	ctx context.Context, repo registry.GitRepo, policyPath string,
) (gitwrite.RepoListing, error) {
	writer, err := v.writer(ctx)
	if err != nil {
		return gitwrite.RepoListing{}, err
	}
	return writer.List(ctx, repo, policyPath)
}

// writer 按**此刻**的设置装一个 gitwrite.Writer。
//
// 摘出来给 Push 与 List 共用：两条路径必须用同一份 host key、同一个凭据后端
// 与同一个超时装出来，各写一遍就会出现「枚举走旧信任锚、推送走新的」这类
// 只在设置刚改过的那一小段时间里才显形的差异。
func (v *settingsPolicyWriter) writer(ctx context.Context) (*gitwrite.Writer, error) {
	s, err := v.settings.Current(ctx)
	if err != nil {
		v.logger.Error("cannot read the platform setting for the policy write-back", "error", err)
		return nil, ErrPolicyWriterUnavailable
	}
	resolver, err := newSecretResolver(ctx, s)
	if err != nil {
		v.logger.Error("cannot build a secret resolver for the policy write-back", "error", err)
		return nil, ErrPolicyWriterUnavailable
	}
	if resolver == nil {
		// 后端是 NONE：操作者明确选了「不解析凭据」，于是也就没有写回。
		return nil, ErrPolicyWriterUnavailable
	}
	// timeout 与 host keys 显式传给 gitwrite.New：它经由 gitssh.New 拒绝
	// 非正超时、也拒绝空 host key 集合。**没有 host key 就构造失败**，
	// 不存在「未配置就不校验」的分支 —— 写路径与读路径共用那一份判定。
	writer, err := gitwrite.New(resolver, []byte(s.GitVerifyHostKeys), s.GitVerifyTimeout)
	if err != nil {
		v.logger.Error("cannot build a policy writer from the current setting", "error", err)
		return nil, ErrPolicyWriterUnavailable
	}
	return writer, nil
}

// 编译期确认它满足边界层要的形状。放在这里而非测试里：接口对不上应当在
// 构建时失败，而不是等到装配时才发现。
var _ httpapi.PolicyWriter = (*settingsPolicyWriter)(nil)

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
