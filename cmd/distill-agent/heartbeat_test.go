package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 刚写下的心跳是「新鲜」的。
func TestAFreshHeartbeatIsLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := writeHeartbeat(path); err != nil {
		t.Fatalf("writeHeartbeat: %v", err)
	}
	if err := checkHeartbeat(path, time.Minute); err != nil {
		t.Errorf("a heartbeat written just now was judged stale: %v", err)
	}
}

// 超过窗口的心跳是「卡死」—— liveness 该据此重启。
//
// 这一条钉住的是探针**真的看时间**：写一个过去的时间戳，它必须报 stale，
// 而不是「文件在就算活」。
func TestAStaleHeartbeatIsDead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	old := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339Nano)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkHeartbeat(path, 10*time.Minute); err == nil {
		t.Error("a heartbeat 30m old passed a 10m liveness window; a wedged agent would never be restarted")
	}
}

// 还没写过心跳（文件不存在）算「没起来」，不是「活着」。
//
// 失败方向朝重启，不朝放行：一个还没写过任何一轮的进程要么在启动、要么
// 卡在启动，两者都不该被当成健康。错误里不带路径（规范 §19、§22）。
func TestAMissingHeartbeatIsNotLive(t *testing.T) {
	const p = "/var/run/distill-agent/heartbeat-never-written"
	err := checkHeartbeat(p, time.Minute)
	if err == nil {
		t.Fatal("a never-written heartbeat was judged live")
	}
	if strings.Contains(err.Error(), p) {
		t.Errorf("the error text carries the heartbeat path: %q", err.Error())
	}
}

// 心跳文件里是垃圾（半截写入、被人动过）算「不可信」，按卡死处理。
func TestAnUnparseableHeartbeatIsNotLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := os.WriteFile(path, []byte("not a timestamp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkHeartbeat(path, time.Minute); err == nil {
		t.Error("an unparseable heartbeat was judged live")
	}
}

// 空路径 = 没开心跳这个功能：writeHeartbeat 是 no-op，不报错。
func TestAnEmptyHeartbeatPathIsANoOp(t *testing.T) {
	if err := writeHeartbeat(""); err != nil {
		t.Errorf("writeHeartbeat(\"\") should be a no-op, got %v", err)
	}
}
