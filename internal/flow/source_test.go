package flow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
)

// addressOnlySource 是一个只拿得到地址的来源（VPC flow logs 那一形态）：
// 不带身份、不报判定、给不出采样率。它必须能满足 flow.Source —— 契约
// 一旦假设流量自带身份，这个来源就只能编一个身份出来。
type addressOnlySource struct{ window flow.Window }

func (s addressOnlySource) Ingest(_ context.Context, clusterID string, window flow.Window) (flow.IngestResult, error) {
	conn := flow.Connection{
		Source:        flow.Endpoint{IP: "10.4.1.7"},
		Dest:          flow.Endpoint{IP: "10.4.2.9"},
		Protocol:      flow.ProtocolTCP,
		Port:          5432,
		ObservedCount: 1,
	}
	if clusterID == "" {
		return flow.IngestResult{}, errors.New("missing cluster")
	}
	return flow.NewIngestResult(flow.SourceHubble, window, s.window, []flow.Connection{conn})
}

// labelledSource 是 Hubble 那一形态：流量自带 Pod 标签，且报告实际判定。
type labelledSource struct{}

func (labelledSource) Ingest(_ context.Context, _ string, window flow.Window) (flow.IngestResult, error) {
	api := identity.Identity{Namespace: "payment", PodName: "api-7d9", WorkloadKind: "Deployment", WorkloadName: "api"}
	db := identity.Identity{Namespace: "payment", PodName: "db-0", WorkloadKind: "StatefulSet", WorkloadName: "db"}
	conn := flow.Connection{
		Source:        flow.Endpoint{IP: "10.4.1.7"}.WithIdentity(api, identity.OutcomeResolved),
		Dest:          flow.Endpoint{IP: "10.4.2.9"}.WithIdentity(db, identity.OutcomeResolved),
		Protocol:      flow.ProtocolTCP,
		Port:          5432,
		ObservedCount: 42,
	}.WithVerdict(flow.VerdictDenied)
	return flow.NewIngestResult(flow.SourceHubble, window, window, []flow.Connection{conn})
}

// TestSourceAdmitsBothFormsOfTraffic 走的是调用方那条路：拿到 IngestResult
// 后必须同时接住连接与完整度，两种来源都不例外。
func TestSourceAdmitsBothFormsOfTraffic(t *testing.T) {
	from := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	window := flow.Window{From: from, To: from.Add(time.Hour)}

	// 只覆盖到半小时的来源：完整度必须自己降下来，不靠调用方记得去查。
	partial := addressOnlySource{window: flow.Window{From: from, To: from.Add(30 * time.Minute)}}

	var sources = map[string]struct {
		src           flow.Source
		wantIdentity  identity.Outcome
		wantVerdict   bool
		wantCompleted flow.Completeness
	}{
		"只有地址": {partial, identity.OutcomeNoData, false, flow.CompletenessDegraded},
		"自带标签": {labelledSource{}, identity.OutcomeResolved, true, flow.CompletenessUnknown},
	}

	for name, tc := range sources {
		res, err := tc.src.Ingest(context.Background(), "prod-a", window)
		if err != nil {
			t.Fatalf("%s: Ingest: %v", name, err)
		}
		conns, completeness := res.Connections()
		if len(conns) != 1 {
			t.Fatalf("%s: 拿到 %d 条连接", name, len(conns))
		}
		if _, outcome := conns[0].Source.Identity(); outcome != tc.wantIdentity {
			t.Errorf("%s: 源端可信度 = %q, want %q", name, outcome, tc.wantIdentity)
		}
		if _, reported := conns[0].Verdict(); reported != tc.wantVerdict {
			t.Errorf("%s: verdict reported = %v, want %v", name, reported, tc.wantVerdict)
		}
		if completeness != tc.wantCompleted {
			t.Errorf("%s: completeness = %q, want %q", name, completeness, tc.wantCompleted)
		}
		// 两种来源都给不出采样率，于是都不得读出 1.0。
		if rate, known := res.SampleRate(); known || rate == 1 {
			t.Errorf("%s: 采样率读出 (%v, %v)，来源根本没报", name, rate, known)
		}
	}
}

// 一次失败的摄入返回的零值结果，不得看起来像"这段时间没有流量"。
func TestFailedIngestIsNotAnEmptyWindow(t *testing.T) {
	from := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	res, err := addressOnlySource{}.Ingest(context.Background(), "", flow.Window{From: from, To: from.Add(time.Hour)})
	if err == nil {
		t.Fatal("缺集群的摄入没有报错")
	}
	conns, completeness := res.Connections()
	if len(conns) != 0 {
		t.Fatalf("失败的摄入带回了 %d 条连接", len(conns))
	}
	if completeness != flow.CompletenessUnknown {
		t.Fatalf("失败的摄入 completeness = %q, want UNKNOWN", completeness)
	}
}
