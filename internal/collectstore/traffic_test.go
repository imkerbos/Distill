package collectstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// 本文件的四个地址各自演一种"归属解不出来"的情形，外加一个从头到尾解得
// 出来的对照组。地址而不是布尔开关：解析结果是区间表按时刻算出来的，
// 用开关伪造等于把被测的那一步换掉。
const (
	// handoverIP 在被描述的窗口**中途**换了主体。
	handoverIP = "10.4.0.7"
	// gapIP 在窗口前后都有主体，唯独窗口那一刻没有。
	gapIP = "10.4.0.8"
	// outsideIP 落在全部已登记网段之外，一个区间都没有。
	outsideIP = "198.51.100.9"
)

// 端口既是这几条连接的区分键，也是断言的索引 —— 一条连接在列表里没了，
// 对应的键就查不到，而不是悄悄换成另一条。
const (
	portResolved = 8080
	portHandover = 8081
	portGap      = 8082
	portOutside  = 8083
	// portDenied 走的是回程方向：shop/web-1 → payment/api-1，被 allow-api 的
	// 入向隔离拦下。端口本身不参与判定（allow-api 一条规则都没有），它只是
	// 让来回两条连接在按端口索引的断言里各自可寻。
	portDenied = 8084
	// portDatabase 是风险端口清单里的 MySQL，供安全发现用。
	portDatabase = 3306
)

// overFindingsCap 刚好比安全发现的清单上限多一条。
//
// 写死而不是从包里导出常量：上限被人调小时这个测试必须仍然证明"越界会被
// 拒绝"，而一个跟着实现走的期望值证明不了任何事。
const overFindingsCap = 1001

var (
	// earlyRunAt 早于第一次采集：gapIP 与 handoverIP 都要有一段窗口之前的历史，
	// 否则 identity.Resolve 分不清"那时这个地址空着"与"那时我们没在看"。
	earlyRunAt = firstRunAt.Add(-time.Hour)
	// handoverRunAt 落在被描述的窗口**内部**，交接因此发生在窗口中途。
	handoverRunAt = windowStart.Add(2 * time.Minute)
)

const (
	uidAPI      = "3c9d2b1a-0000-4000-8000-00000000010a"
	uidWeb      = "3c9d2b1a-0000-4000-8000-00000000010b"
	uidHandover = "3c9d2b1a-0000-4000-8000-00000000010c"
	uidTakeover = "3c9d2b1a-0000-4000-8000-00000000010d"
	uidGapFirst = "3c9d2b1a-0000-4000-8000-00000000010e"
	uidGapAgain = "3c9d2b1a-0000-4000-8000-00000000010f"
)

// stablePods 是两个从头到尾没换过主体的 Pod，每一次采集都要带上它们。
//
// 每次都带，是因为一次采集里没出现的地址会被推导成"区间到此结束" ——
// 少带一次，对照组也会跟着解不出来，于是这份测试再也证明不了任何事。
func stablePods() []snapshot.Pod {
	return []snapshot.Pod{
		podAt("payment", "api-1", uidAPI, recycledIP, "api"),
		podAt("shop", "web-1", uidWeb, peerIP, "web"),
	}
}

func withStable(extra ...snapshot.Pod) []snapshot.Pod {
	return append(stablePods(), extra...)
}

// seedFourSituations 造出四种情形并存的资产时间线：
//
//	T0-1h   handoverIP 属于 handover-1，gapIP 属于 gap-1
//	T0      gapIP 不再出现 —— 它的区间就此关闭
//	T0+5m   被描述的窗口开始
//	T0+7m   handoverIP 换给 handover-2 —— 交接落在窗口内部
//	T0+1h   gapIP 又出现，属于 gap-2 —— 于是观测窗口延续到现在
//
// 窗口起点那一刻：recycledIP / peerIP 解得出且稳定；handoverIP 解得出但在
// 窗口里换过手；gapIP 落在两段区间之间；outsideIP 一个区间都没有。
func seedFourSituations(t *testing.T, s *snapshotstore.Store) {
	t.Helper()
	saveRun(t, s, "run-early", earlyRunAt, withStable(
		podAt("payment", "handover-1", uidHandover, handoverIP, "handover"),
		podAt("payment", "gap-1", uidGapFirst, gapIP, "gap"),
	))
	saveRun(t, s, "run-1", firstRunAt, withStable(
		podAt("payment", "handover-1", uidHandover, handoverIP, "handover"),
	))
	saveRun(t, s, "run-handover", handoverRunAt, withStable(
		podAt("payment", "handover-2", uidTakeover, handoverIP, "handover"),
	))
	saveRun(t, s, "run-2", recycleAt, withStable(
		podAt("payment", "handover-2", uidTakeover, handoverIP, "handover"),
		podAt("payment", "gap-2", uidGapAgain, gapIP, "gap"),
	))
}

// conn 构造一条窗口内的连接。两端都不带身份：来源只报了地址，归属只可能
// 来自区间表。
func conn(srcIP, dstIP string, port int32) flow.Connection {
	return flow.Connection{
		Source:        flow.Endpoint{IP: srcIP},
		Dest:          flow.Endpoint{IP: dstIP},
		Protocol:      flow.ProtocolTCP,
		Port:          port,
		ObservedCount: 3,
	}
}

// describedWindow 是本文件全部查询用的窗口，与摄入窗口一致。
func describedWindow() store.TimeWindow {
	return store.TimeWindow{From: windowStart, To: windowEnd}
}

// saveSampledIngest 落一次**明确报告丢过记录**的摄入。
//
// 采样率照报 1，丢弃报 3：完整度因此是 DEGRADED 而不是 UNKNOWN，两者是两条
// 不同的排查线索（flow.Completeness），而这个用例要的是"知道漏了"那一条。
func saveSampledIngest(t *testing.T, s *snapshotstore.Store, conns []flow.Connection) {
	t.Helper()
	w := flow.Window{From: windowStart, To: windowEnd}
	res, err := flow.NewIngestResult(flow.SourceHubble, w, w, conns)
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	res = res.WithSampleRate(1).WithDropped(3)
	if err := s.SaveIngest(context.Background(), snapshotstore.IngestRun{
		ClusterID:  collectedID,
		RunID:      "ingest-sampled",
		StartedAt:  windowEnd,
		FinishedAt: windowEnd.Add(time.Second),
		Status:     snapshotstore.IngestOK,
		Result:     res,
	}); err != nil {
		t.Fatalf("SaveIngest() error = %v", err)
	}
}

// byPort 按端口索引一页流量，并顺带证明没有一条连接凭空消失。
func byPort(t *testing.T, page store.FlowPage, wantRows int) map[int32]store.FlowRecord {
	t.Helper()
	if page.Total != wantRows {
		t.Fatalf("Flows().Total = %d, want %d: an unattributable connection must not be dropped "+
			"— a smaller observed set makes the rules covering it look unused and safe to tighten",
			page.Total, wantRows)
	}
	if len(page.Items) != wantRows {
		t.Fatalf("Flows() returned %d rows, want %d", len(page.Items), wantRows)
	}
	out := make(map[int32]store.FlowRecord, len(page.Items))
	for _, rec := range page.Items {
		out[rec.Port] = rec
	}
	return out
}

// 无法归属的连接不得消失：它进 UNKNOWN 并带既有的原因取值。
//
// 丢弃会让观测集合小于真实集合，于是覆盖那部分流量的规则被判「无流量、
// 可收紧」（design doc §3）—— 这一层存在的意义就是挡住那个单向的失败方向。
//
// 四种情形各一个子用例，取值全部来自 replay 里那份既有的封闭枚举，一个都
// 不新增。对照组是一条两端都解得出来的连接：少了它，一个「什么都标 UNKNOWN」
// 的实现照样能让下面每一条断言通过。
func TestAnUnattributableConnectionBecomesUnknownNotNothing(t *testing.T) {
	t.Run("窗口完整时，三种归属问题各自落到自己的原因上", func(t *testing.T) {
		r, s := newTestReader(t)
		ctx := context.Background()
		seedFourSituations(t, s)
		saveIngest(t, s, []flow.Connection{
			conn(recycledIP, peerIP, portResolved),
			conn(handoverIP, peerIP, portHandover),
			conn(gapIP, peerIP, portGap),
			conn(recycledIP, outsideIP, portOutside),
		})

		page, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: describedWindow()})
		if err != nil {
			t.Fatalf("Flows() error = %v", err)
		}
		rows := byPort(t, page, 4)

		for _, tc := range []struct {
			name   string
			port   int32
			reason string
		}{
			{"任一端归属不唯一", portHandover, "IP_AMBIGUOUS"},
			// 集群内地址解不出主体 = 我们的数据缺了一块；Fleet 之外的地址
			// 解不出主体 = 对端本来就没有主体。两个原因登记的正是这条分界
			// （replay/decision.go），而按 identity.Outcome 直接映射会把它们
			// 对调：一个真正的公网地址一个区间都没有，解析器答的是 NO_DATA。
			{"集群内地址在那一刻没有主体", portGap, "SNAPSHOT_MISSING"},
			{"对端在 Fleet 之外，本就没有主体", portOutside, "EXTERNAL_NO_IDENTITY"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec, ok := rows[tc.port]
				if !ok {
					t.Fatalf("the connection on port %d is gone from the list", tc.port)
				}
				if rec.Verdict != "UNKNOWN" {
					t.Errorf("Verdict = %s, want UNKNOWN", rec.Verdict)
				}
				if rec.UnknownReason != tc.reason {
					t.Errorf("UnknownReason = %q, want %q", rec.UnknownReason, tc.reason)
				}
			})
		}

		// 对照组：两端都解得出来的那条必须被正常归属 —— 说得出是哪两个
		// 主体，判定不是 UNKNOWN，也不带原因。
		t.Run("对照组：两端都解得出来的连接正常归属", func(t *testing.T) {
			rec, ok := rows[portResolved]
			if !ok {
				t.Fatalf("the connection on port %d is gone from the list", portResolved)
			}
			if rec.Verdict == "UNKNOWN" || rec.UnknownReason != "" {
				t.Fatalf("Verdict = %s / UnknownReason = %q; both ends resolve from the interval "+
					"covering that moment, so this connection must be attributed, not admitted as unknown",
					rec.Verdict, rec.UnknownReason)
			}
			if want := collectedID + "/payment/api-1"; rec.SourceLabel != want {
				t.Errorf("SourceLabel = %q, want %q", rec.SourceLabel, want)
			}
			if want := collectedID + "/shop/web-1"; rec.DestLabel != want {
				t.Errorf("DestLabel = %q, want %q", rec.DestLabel, want)
			}
			if rec.Confidence != "TRUSTED" {
				t.Errorf("Confidence = %q, want TRUSTED: the window is COMPLETE and neither end is "+
					"in a mesh", rec.Confidence)
			}
		})
	})

	// 观测漏了记录**不改写 verdict**：它按 design doc §4 传导成可信度。
	// 与上面三种分开落库 —— 完整度是按窗口算的，一个窗口不可能一半完整
	// 一半不完整。
	t.Run("观测漏了记录时，判定照常给出，只是不可信", func(t *testing.T) {
		r, s := newTestReader(t)
		ctx := context.Background()
		seedFourSituations(t, s)
		saveSampledIngest(t, s, []flow.Connection{
			conn(recycledIP, peerIP, portResolved),
			conn(peerIP, recycledIP, portDenied),
		})

		page, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: describedWindow()})
		if err != nil {
			t.Fatalf("Flows() error = %v", err)
		}
		rows := byPort(t, page, 2)

		// 两端都解得开的连接在 DEGRADED 窗口里必须**照样被判定**。把完整度
		// 写进 verdict 会让这两条都变成 UNKNOWN，于是一个已知漏了记录的窗口
		// 下的预览报「会拦断 0 条」，页面读起来是「可以下发」。
		for _, tc := range []struct {
			port    int32
			verdict string
		}{
			{portResolved, "ALLOW"},
			{portDenied, "DENY"},
		} {
			rec := rows[tc.port]
			if rec.Verdict != tc.verdict {
				t.Errorf("port %d: Verdict = %s, want %s: an incomplete window degrades confidence, "+
					"it does not rewrite a verdict that is computable", tc.port, rec.Verdict, tc.verdict)
			}
			if rec.UnknownReason != "" {
				t.Errorf("port %d: UnknownReason = %q, want empty: both ends resolve",
					tc.port, rec.UnknownReason)
			}
			// 这就是 design doc §4 那句「一次判定可以是『会拦断』且『不可信』」：
			// verdict 与 confidence 始终是两个字段，完整度只碰后者。求值引擎
			// 自己给的是 TRUSTED（无 mesh、无 CCNP），窗口把它压下来。
			if rec.Confidence != "DEGRADED" {
				t.Errorf("port %d: Confidence = %q, want DEGRADED: this window is known to have "+
					"dropped records, so every conclusion drawn from it is untrusted",
					tc.port, rec.Confidence)
			}
		}
	})
}

// 判定必须来自那一次真实求值，**方向包含在内**。
//
// 两条连接是同一对端点的一来一回：payment/api-1 与 shop/web-1。种子里的
// allow-api 只选中 payment 里 app=api 的 Pod、且一条 ingress 规则都没有，
// 按 K8s 语义它对 api-1 是「入向隔离 + 零规则」= 拒绝一切入向，出向不管。
// 于是方向是**承重的**：
//
//   - api-1 → web-1：出向不隔离、shop 无策略 → ALLOW
//   - web-1 → api-1：命中 api-1 的入向隔离、无规则放行 → DENY
//
// 把 Evaluate 的 Source / Dest 对调，两条断言同时红；把整句求值换成任何一个
// 常量，也至少红一条。少了这个用例，一条真会被入向策略拦断的连接被判成
// ALLOW，预览页的「会拦断 N 条」少算 N —— 本平台唯一那个要命的方向。
func TestTheVerdictComesFromTheEvaluationInTheDirectionObserved(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	seedFourSituations(t, s)
	saveIngest(t, s, []flow.Connection{
		conn(recycledIP, peerIP, portResolved),
		conn(peerIP, recycledIP, portDenied),
	})

	page, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: describedWindow()})
	if err != nil {
		t.Fatalf("Flows() error = %v", err)
	}
	rows := byPort(t, page, 2)

	out := rows[portResolved]
	if out.SourceLabel != collectedID+"/payment/api-1" || out.DestLabel != collectedID+"/shop/web-1" {
		t.Fatalf("port %d = %s → %s, want payment/api-1 → shop/web-1",
			portResolved, out.SourceLabel, out.DestLabel)
	}
	if out.Verdict != "ALLOW" {
		t.Errorf("payment/api-1 → shop/web-1 = %s, want ALLOW: allow-api does not isolate egress and "+
			"shop carries no policy", out.Verdict)
	}

	in := rows[portDenied]
	if in.SourceLabel != collectedID+"/shop/web-1" || in.DestLabel != collectedID+"/payment/api-1" {
		t.Fatalf("port %d = %s → %s, want shop/web-1 → payment/api-1",
			portDenied, in.SourceLabel, in.DestLabel)
	}
	if in.Verdict != "DENY" {
		t.Errorf("shop/web-1 → payment/api-1 = %s, want DENY: allow-api isolates api-1 on ingress and "+
			"carries no rule that admits anyone — reversing the two ends is what this asserts", in.Verdict)
	}
	if in.Confidence != "TRUSTED" {
		t.Errorf("Confidence = %q, want TRUSTED: the window is COMPLETE and neither end is in a mesh",
			in.Confidence)
	}

	// 理由也要绑住：结论从哪个方向来、主体是不是真的进了隔离。只断言 DENY
	// 的话，一个"入向永远 DENY"的实现照样能过，而它对出向隔离一无所知。
	got, ok, err := r.Flow(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("Flow(%s) = ok %v, err %v", in.ID, ok, err)
	}
	if got.Reason.Direction != "INGRESS" {
		t.Errorf("Reason.Direction = %q, want INGRESS: it is the ingress side of payment/api-1 that "+
			"decides this connection", got.Reason.Direction)
	}
	if !got.Reason.Isolated {
		t.Errorf("Reason.Isolated = false; allow-api selects api-1 and therefore isolates it on ingress")
	}
}

// 列表里的每一行都要能被下钻，而下钻到的必须是同一条连接的同一次判定。
//
// 同时守住条数上限：调用方选的 limit 不得无界（安全规范 §23 / §24），而
// 截断必须可见 —— Total 与 Limit 都要照实回显，否则界面会把「我只给你看了
// 这些」显示成「一共就这些」。
func TestAFlowListIsBoundedAndEveryRowCanBeDrilledInto(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	seedFourSituations(t, s)
	saveIngest(t, s, []flow.Connection{
		conn(recycledIP, peerIP, portResolved),
		conn(handoverIP, peerIP, portHandover),
		conn(gapIP, peerIP, portGap),
		conn(recycledIP, outsideIP, portOutside),
	})
	window := describedWindow()

	capped, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: window, Limit: 1 << 30})
	if err != nil {
		t.Fatalf("Flows() error = %v", err)
	}
	if capped.Limit != 1000 {
		t.Errorf("Flows(limit=2^30).Limit = %d, want the reader's own ceiling 1000: a caller-chosen "+
			"page size must not be unbounded", capped.Limit)
	}

	truncated, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: window, Limit: 2})
	if err != nil {
		t.Fatalf("Flows() error = %v", err)
	}
	if len(truncated.Items) != 2 || truncated.Total != 4 || truncated.Limit != 2 {
		t.Fatalf("Flows(limit=2) = %d items / Total %d / Limit %d, want 2 / 4 / 2",
			len(truncated.Items), truncated.Total, truncated.Limit)
	}

	page, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: window})
	if err != nil {
		t.Fatalf("Flows() error = %v", err)
	}
	for _, want := range page.Items {
		got, ok, err := r.Flow(ctx, want.ID)
		if err != nil {
			t.Fatalf("Flow(%s) error = %v", want.ID, err)
		}
		if !ok {
			t.Fatalf("Flow(%s) not found; every row in the list must be drillable", want.ID)
		}
		if got.FlowRecord != want {
			t.Errorf("Flow(%s) = %+v, want the very row the list showed %+v", want.ID, got.FlowRecord, want)
		}
		if got.Verdict == "UNKNOWN" && got.Reason.MatchedRuleIdx != -1 {
			t.Errorf("Flow(%s) points at rule %d while deciding nothing", want.ID, got.Reason.MatchedRuleIdx)
		}
	}

	// 认不出来的 ID 答"没有这条"，不报错：一个解不开的 ID 与一条不存在的
	// 流量对调用方是同一件事，分开报会让人靠响应差异探出 ID 的结构。
	if _, ok, err := r.Flow(ctx, "not-a-flow-id"); err != nil || ok {
		t.Errorf("Flow(bogus) = ok %v, err %v; want not found without an error", ok, err)
	}
}

// 完整度只降不升：一个 COMPLETE 窗口不得把求值引擎已经降下来的可信度提回去。
//
// 种子与上一个用例逐字相同，只有一处不同：集群登记了 CCNP。于是求值引擎自己
// 答 DEGRADED（replay.confidenceFor），而窗口是 COMPLETE（saveIngest 报
// dropped=0）—— 两个来源第一次给出不同的答案，"只降不升"这句话才有内容。
//
// 少了它，一个把 Confidence 无条件写成 confidenceOf(窗口完整度) 的实现全绿：
// 那样一来「这个集群装着 CCNP、标准 NetworkPolicy 的结论可能是看起来对的错」
// 会被「这段观测没问题」整个覆盖掉，而可信度这个字段存在的第一个理由正是它。
func TestACompleteWindowDoesNotPromoteADegradedEvaluation(t *testing.T) {
	r, s := newTestReaderWithSource(t, ccnpSource())
	ctx := context.Background()
	seedFourSituations(t, s)
	saveIngest(t, s, []flow.Connection{
		conn(recycledIP, peerIP, portResolved),
		conn(peerIP, recycledIP, portDenied),
	})

	page, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: describedWindow()})
	if err != nil {
		t.Fatalf("Flows() error = %v", err)
	}
	rows := byPort(t, page, 2)

	for _, tc := range []struct {
		port    int32
		verdict string
	}{
		{portResolved, "ALLOW"},
		{portDenied, "DENY"},
	} {
		rec := rows[tc.port]
		// 判定照旧 —— CCNP 降的是可信度，不是结论（replay.confidenceFor）。
		// 同一对端点在没有 CCNP 时的判定见
		// TestTheVerdictComesFromTheEvaluationInTheDirectionObserved。
		if rec.Verdict != tc.verdict {
			t.Errorf("port %d: Verdict = %s, want %s: CCNP degrades confidence, it decides nothing",
				tc.port, rec.Verdict, tc.verdict)
		}
		if rec.Confidence != "DEGRADED" {
			t.Errorf("port %d: Confidence = %q, want DEGRADED: the evaluator already degraded this "+
				"cluster for CCNP, and a COMPLETE window must not promote it back to TRUSTED",
				tc.port, rec.Confidence)
		}
	}
}

// 同一个集群同一段窗口，质量页与列表页必须对得上账。
//
// 两屏各算各的时，/flows 会显示一条 ALLOW，/quality 同时报 UnknownRate = 1.0
// 且这条落进 UNSPECIFIED —— 运维照着一份自己对不上的账排查，而没有任何一屏
// 说得出该信哪一边。这里把两者钉在同一次判定上：UNKNOWN 的条数、每个原因的
// 构成、以及那条判得出来的连接，三处都必须一致。
func TestQualityAndTheFlowListAgreeOnOneWindow(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	seedFourSituations(t, s)
	saveIngest(t, s, []flow.Connection{
		conn(recycledIP, peerIP, portResolved),
		conn(handoverIP, peerIP, portHandover),
		conn(gapIP, peerIP, portGap),
		conn(recycledIP, outsideIP, portOutside),
	})

	page, err := r.Flows(ctx, store.FlowFilter{Cluster: collectedID, Window: describedWindow()})
	if err != nil {
		t.Fatalf("Flows() error = %v", err)
	}
	q, err := r.Quality(ctx, collectedID)
	if err != nil {
		t.Fatalf("Quality() error = %v", err)
	}

	wantUnknown := 0
	wantComposition := map[string]int{}
	decided := 0
	for _, rec := range page.Items {
		if rec.Verdict != "UNKNOWN" {
			decided++
			continue
		}
		wantUnknown++
		wantComposition[rec.UnknownReason]++
	}
	if decided == 0 {
		t.Fatalf("Flows() decided nothing; without a decided row this test cannot tell the two "+
			"screens apart — every implementation agrees on an all-UNKNOWN window: %+v", page.Items)
	}

	if q.TotalFlows != page.Total {
		t.Errorf("Quality().TotalFlows = %d, Flows().Total = %d; both describe the same window",
			q.TotalFlows, page.Total)
	}
	if q.UnknownCount != wantUnknown {
		t.Errorf("Quality().UnknownCount = %d, but the flow list shows %d UNKNOWN rows in the same "+
			"window; two screens must not disagree about one window", q.UnknownCount, wantUnknown)
	}
	if q.UnknownRate >= 1 {
		t.Errorf("Quality().UnknownRate = %v while the list decided %d of %d rows",
			q.UnknownRate, decided, page.Total)
	}
	for reason, want := range wantComposition {
		if got := q.UnknownComposition[reason]; got != want {
			t.Errorf("UnknownComposition[%s] = %d, want %d (the flow list's own count)", reason, got, want)
		}
	}
	if got := q.UnknownComposition["UNSPECIFIED"]; got != 0 {
		t.Errorf("UnknownComposition[UNSPECIFIED] = %d, want 0: every UNKNOWN row above carries a "+
			"registered reason, so nothing may land in the catch-all bucket", got)
	}
}

// 安全发现的三个清单都必须有上限，而上限必须是可见的。
//
// 一个把数据库端口用满的集群会让一次 /security 在内存里组出几万条风险流量
// 并整份序列化（design doc §9 点名的唯一失控方向，安全规范 §23 / §24）。
// 这里选择超出即拒绝而不是截断：一份被悄悄截到 1000 条的风险清单读起来
// 就是「这个集群只有这些风险」，而报告上没有任何字段能说出它被截过。
func TestASecurityReportRefusesToShowATruncatedList(t *testing.T) {
	// 三个清单各有各的上限，一条都不能少：只测其中一个，另外两个的上限
	// 被删掉时全套仍然全绿。
	assertRefused := func(t *testing.T, rep store.SecurityReport, err error, list string) {
		t.Helper()
		if !errors.Is(err, collectstore.ErrTooManyFindings) {
			t.Fatalf("Security() error = %v, want ErrTooManyFindings: more than %d %s must not come "+
				"back as one response", err, overFindingsCap-1, list)
		}
		if len(rep.RiskyFlows) != 0 || len(rep.EgressTargets) != 0 || len(rep.NakedPods) != 0 {
			t.Errorf("Security() returned %d risky flows / %d egress targets / %d naked pods alongside "+
				"the refusal; a partial report is exactly the thing being refused",
				len(rep.RiskyFlows), len(rep.EgressTargets), len(rep.NakedPods))
		}
	}

	t.Run("风险流量", func(t *testing.T) {
		r, s := newTestReader(t)
		seedFourSituations(t, s)
		// 源地址各不相同，事实层才会把它们当成不同的连接；目的地址在集群
		// 网段内，于是越界的只可能是风险清单这一个。
		conns := make([]flow.Connection, 0, overFindingsCap)
		for i := range overFindingsCap {
			conns = append(conns, conn(fmt.Sprintf("10.4.%d.%d", 200+i/256, i%256), peerIP, portDatabase))
		}
		saveIngest(t, s, conns)

		rep, err := r.Security(context.Background(), collectedID, describedWindow())
		assertRefused(t, rep, err, "connections on a risky port")
	})

	t.Run("出网目标", func(t *testing.T) {
		r, s := newTestReader(t)
		seedFourSituations(t, s)
		// 目的地址各不相同且全在已登记网段之外，端口不在风险清单里：
		// 越界的只可能是出网目标这一个。
		conns := make([]flow.Connection, 0, overFindingsCap)
		for i := range overFindingsCap {
			conns = append(conns, conn(recycledIP, fmt.Sprintf("198.51.%d.%d", i/256, i%256), portOutside))
		}
		saveIngest(t, s, conns)

		rep, err := r.Security(context.Background(), collectedID, describedWindow())
		assertRefused(t, rep, err, "distinct addresses outside every registered range")
	})

	t.Run("裸奔 Pod", func(t *testing.T) {
		r, s := newTestReader(t)
		// 裸奔清单来自锚点那次快照，与流量窗口无关，因此这里要的是一次
		// 装着大量未被任何策略选中的 Pod 的采集。
		naked := make([]snapshot.Pod, 0, overFindingsCap)
		for i := range overFindingsCap {
			naked = append(naked, podAt("batch", fmt.Sprintf("naked-%d", i),
				fmt.Sprintf("3c9d2b1a-0000-4000-8000-%012d", i),
				fmt.Sprintf("10.4.%d.%d", 100+i/256, i%256), "naked"))
		}
		saveRun(t, s, "run-naked", firstRunAt, withStable(naked...))
		saveIngest(t, s, []flow.Connection{conn(recycledIP, peerIP, portResolved)})

		rep, err := r.Security(context.Background(), collectedID, describedWindow())
		assertRefused(t, rep, err, "pods not selected by any policy")
	})
}

// 安全发现必须把归属不明的那些一起报出来，并点名裸奔的 Pod。
//
// 一条判不出结论的数据库直连仍然意味着有人在尝试连数据库：把它从风险清单
// 里滤掉，等于让唯一值得看的那条记录消失。
func TestTheSecurityReportKeepsWhatItCouldNotAttribute(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	seedFourSituations(t, s)
	saveIngest(t, s, []flow.Connection{
		conn(handoverIP, peerIP, portDatabase),
		conn(recycledIP, outsideIP, portOutside),
	})

	rep, err := r.Security(ctx, collectedID, describedWindow())
	if err != nil {
		t.Fatalf("Security() error = %v", err)
	}
	if len(rep.RiskyFlows) != 1 {
		t.Fatalf("RiskyFlows = %+v, want the one database connection even though its source "+
			"could not be attributed", rep.RiskyFlows)
	}
	risky := rep.RiskyFlows[0]
	if risky.Port != portDatabase || risky.Category != store.RiskDatabase {
		t.Errorf("RiskyFlows[0] = port %d / category %s, want %d / %s",
			risky.Port, risky.Category, portDatabase, store.RiskDatabase)
	}
	if risky.UnknownReason != "IP_AMBIGUOUS" {
		t.Errorf("RiskyFlows[0].UnknownReason = %q, want IP_AMBIGUOUS", risky.UnknownReason)
	}
	if risky.Position != store.PositionCrossNamespace {
		t.Errorf("RiskyFlows[0].Position = %s, want %s: an endpoint we cannot attribute must not be "+
			"read as a normal in-namespace call", risky.Position, store.PositionCrossNamespace)
	}

	if len(rep.EgressTargets) != 1 {
		t.Fatalf("EgressTargets = %+v, want the one address outside every registered range",
			rep.EgressTargets)
	}
	egress := rep.EgressTargets[0]
	if egress.Address != outsideIP || egress.FlowCount != 1 || egress.UnknownCount != 1 {
		t.Errorf("EgressTargets[0] = %+v, want %s with 1 flow, 1 of them undecided",
			egress, outsideIP)
	}
	if egress.AllowedCount != 0 {
		t.Errorf("EgressTargets[0].AllowedCount = %d; the far end has no identity, so no rule was "+
			"replayed against it", egress.AllowedCount)
	}

	// 裸奔 Pod 来自锚点那次快照，与流量窗口无关。payment/api-1 被 allow-api
	// 选中，另外两个没有 —— hostNetwork 的一个都没有，不涉及那条排除。
	got := map[string]bool{}
	for _, p := range rep.NakedPods {
		got[p.Namespace+"/"+p.Name] = true
	}
	for _, want := range []string{"payment/handover-1", "shop/web-1"} {
		if !got[want] {
			t.Errorf("NakedPods = %+v, want it to name %s", rep.NakedPods, want)
		}
	}
	if got["payment/api-1"] {
		t.Errorf("NakedPods names payment/api-1, which allow-api selects at that moment")
	}
	if len(rep.RiskPortCatalog) == 0 {
		t.Error("RiskPortCatalog is empty; the report must say what it was looking for")
	}
}
