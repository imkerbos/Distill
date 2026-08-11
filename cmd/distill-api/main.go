// Command distill-api 提供 Distill 平台的 HTTP 接口。
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/buildinfo"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/httpapi"
	applog "github.com/imkerbos/Distill/internal/log"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/response"
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
	if err := mysqlregistry.Migrate(cfg.Database, "migrations"); err != nil {
		return err
	}

	reg := mysqlregistry.New(db)
	// 集群列表在启动时读取一次。后台新增集群后需要重启才生效 ——
	// 本轮如此，因为 FixtureReader 持有的是快照而非查询接口。
	// 接真实存储时 reader 直接查库，这个限制随之消失。
	clusters, err := reg.Clusters(ctx)
	if err != nil {
		return err
	}
	reader := store.NewFixtureReader(fixture.Load(), clusters)

	r := chi.NewRouter()
	r.Mount("/healthz", newHealthHandler())
	r.Mount("/", httpapi.NewRouter(httpapi.Deps{
		Sessions: auth.NewSessionStore(cfg.Auth.SessionTTL, nil),
		Verifier: auth.NewVerifier(cfg.Auth.Users),
		Logger:   logger,
		Reader:   reader,
		Registry: reg,
		// demo 的默认时间窗取 fixture 数据的实际范围。任何"最近 N 天"
		// 的取值都会随真实时间推移而在某天返回 0 条 —— demo 会在没有
		// 人改动代码的情况下自己坏掉。接真实存储时这里换成有界窗口。
		DefaultWindow: reader.DataWindow(),
	}))

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
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
