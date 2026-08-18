package snapshotstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// Pod 的 metrics 抓取声明必须原样往返。
//
// 它是 METRICS_SCRAPE Baseline 依据的一半（design doc 2026-08-18 §3）。
// 静默丢掉这一列的症状是：那一类 Baseline 永远报「缺失」，而运维照着提示去
// 补登记也不会有任何变化 —— 依据在采集时拿到了，在落库时没了。
func TestScrapeAnnotationsSurviveTheRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	at := time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)

	pod := samplePod(clusterA, "web-1", "10.4.0.9")
	pod.ScrapeAnnotations = map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "9102",
		"prometheus.io/path":   "/metrics",
	}
	run := sampleRun(clusterA, "run-scrape", at)
	run.Observation.Pods = []snapshot.Pod{pod}
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var raw string
	if err := db.QueryRow(
		`SELECT scrape_annotations FROM observed_pod
		  WHERE cluster_id = ? AND run_id = ? AND name = ?`,
		clusterA, "run-scrape", "web-1").Scan(&raw); err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, want := range []string{"prometheus.io/scrape", "9102", "/metrics"} {
		if !contains(raw, want) {
			t.Errorf("stored scrape_annotations = %s, want it to contain %q", raw, want)
		}
	}
}

// 没有声明的 Pod 落一个空对象，不落 NULL。
//
// 空对象与 NULL 在读取侧是两条路径，而「这个 Pod 没声明过」只有一种含义。
// 留一个 NULL 会让读取侧多一个必须处理的分支，而漏处理它就是一次 panic。
func TestAPodWithoutScrapeAnnotationsStoresAnEmptyObject(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	at := time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)

	if err := s.Save(ctx, sampleRun(clusterA, "run-plain", at)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	var isNull bool
	if err := db.QueryRow(
		`SELECT scrape_annotations IS NULL FROM observed_pod
		  WHERE cluster_id = ? AND run_id = ? LIMIT 1`,
		clusterA, "run-plain").Scan(&isNull); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if isNull {
		t.Error("scrape_annotations stored as NULL; want an empty JSON object")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
