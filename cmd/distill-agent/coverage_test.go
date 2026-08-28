package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeProc 造一份假的 /proc：超时配置、丢弃计数、表用量。
// lifetimeSeconds 给最短那一项，其余给一个足够长的值。
func fakeProc(t *testing.T, lifetimeSeconds int, drops uint64, count, max int) string {
	t.Helper()
	root := t.TempDir()
	sys := filepath.Join(root, "sys/net/netfilter")
	if err := os.MkdirAll(sys, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i, name := range timeoutFiles {
		v := 600
		if i == 0 {
			v = lifetimeSeconds
		}
		write(t, filepath.Join(sys, name), strconv.Itoa(v))
	}
	write(t, filepath.Join(sys, "nf_conntrack_count"), strconv.Itoa(count))
	write(t, filepath.Join(sys, "nf_conntrack_max"), strconv.Itoa(max))

	stat := filepath.Join(root, "net/stat")
	if err := os.MkdirAll(stat, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, filepath.Join(stat, "nf_conntrack"),
		"entries drop early_drop insert_failed\n"+
			"00000001 "+strconv.FormatUint(drops, 16)+" 00000000 00000000\n")
	return root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// 三项证据齐全时，agent 才填 coveredWindow / sampleRate / dropped。
// 这三个字段是平台判 COMPLETE 的全部依据，缺一个就落回 UNKNOWN。
func TestConntrackClaimsCompletenessOnlyWithProof(t *testing.T) {
	sink := &recordingFlowSink{}
	// 表项最短活 10s，轮询间隔 1ms —— 余量远超 2 倍；无丢弃；表用量 1%。
	err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, twoConnections),
		polls: 2, interval: time.Millisecond,
		procRoot: fakeProc(t, 10, 0, 1000, 131072),
	}, sink, quietLogger(t))
	if err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}

	got := sink.only(t)
	if got.CoveredWindow == nil {
		t.Fatal("证据齐全却没报 coveredWindow —— 平台据此永远判不出 COMPLETE")
	}
	if got.SampleRate == nil || *got.SampleRate != 1 {
		t.Errorf("sampleRate = %v, want 1", got.SampleRate)
	}
	if got.Dropped == nil || *got.Dropped != 0 {
		t.Errorf("dropped = %v, want 0", got.Dropped)
	}
	if !got.CoveredWindow.From.Equal(got.RequestedWindow.From) ||
		!got.CoveredWindow.To.Equal(got.RequestedWindow.To) {
		t.Errorf("覆盖窗口与请求窗口不一致: %+v vs %+v", got.CoveredWindow, got.RequestedWindow)
	}
}

// 任一条前提不成立就什么都不说 —— 与这段代码不存在时完全一样。
// **这是本次改动的安全边界**：它保证默认答案仍然是"证明不了"。
func TestConntrackStaysSilentWithoutProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		proc func(t *testing.T) string
	}{
		{"表项存活不足两倍轮询间隔", func(t *testing.T) string {
			// 间隔 1ms，需要 >= 2ms；给 0 秒读不出来即视为不足。
			return fakeProc(t, 0, 0, 1000, 131072)
		}},
		{"读不到丢弃计数", func(t *testing.T) string {
			// sysctl 都在，只是没有 net/stat/nf_conntrack。
			// "说不出丢没丢"与"说得出没丢"不是一档。
			root := fakeProc(t, 10, 0, 1000, 131072)
			if err := os.Remove(filepath.Join(root, "net/stat/nf_conntrack")); err != nil {
				t.Fatalf("remove: %v", err)
			}
			return root
		}},
		{"表快满了", func(t *testing.T) string {
			return fakeProc(t, 10, 0, 130000, 131072)
		}},
		{"读不到 /proc", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "nonexistent")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingFlowSink{}
			if err := conntrackOnce(context.Background(), conntrackOptions{
				clusterID: "uat", tablePath: tableFile(t, twoConnections),
				polls: 2, interval: time.Millisecond, procRoot: tc.proc(t),
			}, sink, quietLogger(t)); err != nil {
				t.Fatalf("conntrackOnce() error = %v", err)
			}
			got := sink.only(t)
			if got.CoveredWindow != nil {
				t.Errorf("%s，却仍然报了 coveredWindow", tc.name)
			}
			if got.SampleRate != nil {
				t.Errorf("%s，却仍然报了 sampleRate", tc.name)
			}
			if got.Dropped != nil {
				t.Errorf("%s，却仍然报了 dropped=%v", tc.name, *got.Dropped)
			}
		})
	}
}

// 开机以来累计过丢弃，不等于**这个窗口**漏了。完整度看的是窗口内的增量：
// 拿累计数作答，一个跑了一周、早期丢过一次的节点会永远说自己不完整，
// 而那等于这条路径从来没打开过。
func TestAccumulatedDropsFromBeforeTheWindowDoNotBlockIt(t *testing.T) {
	sink := &recordingFlowSink{}
	if err := conntrackOnce(context.Background(), conntrackOptions{
		clusterID: "uat", tablePath: tableFile(t, twoConnections),
		polls: 2, interval: time.Millisecond,
		procRoot: fakeProc(t, 10, 4096, 1000, 131072),
	}, sink, quietLogger(t)); err != nil {
		t.Fatalf("conntrackOnce() error = %v", err)
	}
	if sink.only(t).CoveredWindow == nil {
		t.Error("窗口内零丢弃，却因为历史累计数被判成不完整")
	}
}
