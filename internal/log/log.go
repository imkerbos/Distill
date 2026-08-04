// Package log 构造平台使用的结构化日志器。
package log

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New 构造一个输出 JSON 的日志器。
//
// 固定 JSON 而非可切换格式：日志由 Cloud Logging 解析，
// 一旦允许纯文本，排查线上问题时就会遇到解析不了的那一条。
func New(level string, w io.Writer) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToUpper(level) {
	case "DEBUG":
		lv = slog.LevelDebug
	case "INFO":
		lv = slog.LevelInfo
	case "WARN":
		lv = slog.LevelWarn
	case "ERROR":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}

	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv})), nil
}
