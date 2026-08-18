// Command distill-agent 跑在被管集群里，采集本集群资产并推回平台。
//
// **与 cmd/distill-collector 是两个二进制，这是刻意的**（design doc
// 2026-08-18 §1.2）：这个可执行文件会被装进别人的集群，因此它不得带着
// 平台状态库的访问路径。拆开之后这条性质是机械可查的
// （scripts/check-push-purity.sh），而不是靠 review 记得。
//
// 它拿得到的只有两样东西：本集群的 in-cluster ServiceAccount（只读），
// 以及一把只能往一个集群写数据的 token。平台从来没有见过前者。
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	applog "github.com/imkerbos/Distill/internal/log"
)

// defaultRunTimeout 是一次运行的默认上限。
const defaultRunTimeout = 10 * time.Minute

func main() {
	platformURL := flag.String("platform-url", "", "base URL of the platform")
	tokenFile := flag.String("token-file", "", "path to the mounted agent token")
	timeout := flag.Duration("timeout", defaultRunTimeout, "upper bound on one collection run")
	flag.Parse()

	if err := dispatch(options{platformURL: *platformURL, tokenFile: *tokenFile}, *timeout); err != nil {
		// 日志器可能尚未构造成功，直接写 stderr。
		_, _ = os.Stderr.WriteString("agent run failed: " + err.Error() + "\n")
		os.Exit(1)
	}
}

// dispatch 校验参数并跑一次。
//
// 非正超时不是「不限时」，是一个写错了的配置：默认值只在没给 -timeout 时
// 生效，显式给一个 0 必须被拒绝而不是静默变成无限等待。
func dispatch(opts options, timeout time.Duration) error {
	if timeout <= 0 {
		return errTimeoutNotPositive
	}
	logger, err := applog.New("INFO", os.Stdout)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, opts, timeout, logger)
}
