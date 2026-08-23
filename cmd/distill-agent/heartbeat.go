package main

import (
	"errors"
	"os"
	"strings"
	"time"
)

// 存活探针的心跳。
//
// **心跳记的是「flow 循环转了一圈」，不是「上报成功了」。** 这个区别不是
// 细节：conntrack 读不到时 agent 故意持续推 FAILED + UNREACHABLE —— 那是
// 「这个集群的数据面绕开了 netfilter」这个结论的依据（design doc
// 2026-08-19-conntrack-source §5）。若心跳挂在上报成功上，这类集群会被
// liveness 反复重启，把一个明确的信号变成 CrashLoop。因此心跳只探一件事：
// 这个 goroutine 有没有卡死。
//
// 用内容里的时间戳而不是文件 mtime：一部分卷的 mtime 粒度很粗、或被工具
// 顺手改过，而内容是这个进程自己写的、只有它会写。

// writeHeartbeat 记下「flow 循环刚转到这一圈的顶」。
//
// 空路径是 no-op：没配 -heartbeat-file 就没开这个功能。
//
// 先写临时文件再 rename：探针在另一个进程里读同一个文件（exec 探针跑的是
// 同一个镜像），直接覆盖写会让它读到半截内容、解析失败、误判卡死。rename
// 在同一文件系统上是原子的。
func writeHeartbeat(path string) error {
	if path == "" {
		return nil
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(stamp), 0o600); err != nil {
		return errors.New("the heartbeat file could not be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.New("the heartbeat file could not be replaced")
	}
	return nil
}

// checkHeartbeat 判断 flow 循环是否还在按时转。存活即 nil，卡死即 error。
//
// **三种情形都判「不活」，失败方向一致朝重启**：文件不存在（还没转过一圈，
// 要么在启动、要么卡在启动）、内容解析不出（半截写入或被人动过，不可信）、
// 时间戳超出窗口（转过，但停在很久以前）。让人放心的那个读法——「文件在
// 就算活」——正是危险的那个。
//
// 错误里不带路径：那是部署布局信息，而探针的输出会进 kubelet 事件（规范
// §19、§22）。
func checkHeartbeat(path string, staleAfter time.Duration) error {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path comes from this process's own flags, not a request.
	if err != nil {
		return errors.New("the agent has not written a heartbeat yet")
	}
	last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return errors.New("the heartbeat file does not hold a valid timestamp")
	}
	if time.Since(last) > staleAfter {
		return errors.New("the agent's flow loop has not advanced within the liveness window")
	}
	return nil
}
