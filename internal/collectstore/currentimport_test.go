package collectstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/store"
)

// denyWebIngress 是一条"平台采不到、但集群里真的有"的策略：
// 对 shop/web 的 default-deny ingress。
//
// **刻意选 shop/web，不选 payment/api**：种子里那条 allow-api 已经是对
// payment/api 的 default-deny ingress，拿它做对象的话判定本来就是 DENY，
// 三条用例会一起假绿（第一版就是这么写的，跑出来才发现）。shop/web 不被
// 任何种子策略选中，因此判定只取决于这条导入进没进来。
const denyWebIngress = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: unseen-deny-web
  namespace: shop
spec:
  podSelector:
    matchLabels:
      app: web
  policyTypes:
  - Ingress
`

// baselineImport 造一条 RoleBaselineCurrent 的导入。
func baselineImport(id string, at time.Time) registry.PolicyImport {
	return registry.PolicyImport{
		ClusterID: collectedID, ImportID: id,
		Role: registry.RoleBaselineCurrent, Source: registry.SourcePaste,
		Namespace: "shop", Name: "unseen-deny-web",
		YAML: denyWebIngress, ImportedAt: at,
	}
}

// **导入的"现状"策略并进当前判定。**
//
// 它补的是平台采不到的那部分：某个 namespace 没有 list 权限、或者策略由别的
// 系统管着。不并进来，平台会以为那些连接没有任何策略约束，于是判 ALLOW ——
// 而集群里它们其实是被拦着的。
func TestBaselineCurrentImportsAffectTheCurrentVerdict(t *testing.T) {
	// 导入时刻早于窗口起点，因此这个窗口用得上它。
	before := windowStart.Add(-time.Hour)
	r, s := newTestReaderWithImports(t, map[string][]registry.PolicyImport{
		collectedID: {baselineImport("imp-current", before)},
	})
	seedPreviewCluster(t, s)
	// 方向刻意反过来：payment/api -> shop/web。种子里那条 allow-api 只管
	// payment/api 的 ingress，管不到这条，因此判定只取决于导入。
	saveIngest(t, s, []flow.Connection{conn(recycledIP, peerIP, portResolved)})

	flows, err := r.Flows(context.Background(), store.FlowFilter{
		Cluster: collectedID, Window: describedWindow(),
	})
	if err != nil {
		t.Fatalf("Flows() = %v", err)
	}
	if len(flows.Items) == 0 {
		t.Fatal("没有流量，这条用例等于没跑")
	}
	for _, f := range flows.Items {
		if f.Verdict != string(replay.VerdictDeny) {
			t.Errorf("到 shop/web 的连接判定 = %s, want DENY —— "+
				"导入的 default-deny 没有进当前策略集", f.Verdict)
		}
	}
}

// **导入时刻晚于窗口起点时，这个窗口不受它影响。**
//
// 一次导入能证明的只是"我在这个时刻告诉你集群里有这条策略"，它没有说过那条
// 策略更早就存在。拿它去解释更早的窗口，等于替人补上一句他没说过的话。
func TestBaselineCurrentImportsDoNotExplainEarlierWindows(t *testing.T) {
	// 导入发生在窗口**之后**。
	after := windowEnd.Add(time.Hour)
	r, s := newTestReaderWithImports(t, map[string][]registry.PolicyImport{
		collectedID: {baselineImport("imp-late", after)},
	})
	seedPreviewCluster(t, s)
	// 方向刻意反过来：payment/api -> shop/web。种子里那条 allow-api 只管
	// payment/api 的 ingress，管不到这条，因此判定只取决于导入。
	saveIngest(t, s, []flow.Connection{conn(recycledIP, peerIP, portResolved)})

	flows, err := r.Flows(context.Background(), store.FlowFilter{
		Cluster: collectedID, Window: describedWindow(),
	})
	if err != nil {
		t.Fatalf("Flows() = %v", err)
	}
	if len(flows.Items) == 0 {
		t.Fatal("没有流量，这条用例等于没跑")
	}
	for _, f := range flows.Items {
		if f.Verdict == string(replay.VerdictDeny) {
			t.Errorf("一条导入时刻晚于这个窗口的策略改变了这个窗口的判定（%s）——"+
				"那是替人补上了一句他没说过的话", f.Verdict)
		}
	}
}

// **CANDIDATE_ADDITION 不进当前判定。**
//
// 那个角色描述的是"我希望平台补上这条放行"，不是"集群里现在有这条策略"。
// 混进当前策略集，会让平台把一条还没下发的规则当成正在生效的。
func TestCandidateAdditionImportsStayOutOfTheCurrentVerdict(t *testing.T) {
	before := windowStart.Add(-time.Hour)
	imp := baselineImport("imp-addition", before)
	imp.Role = registry.RoleCandidateAddition

	r, s := newTestReaderWithImports(t, map[string][]registry.PolicyImport{
		collectedID: {imp},
	})
	seedPreviewCluster(t, s)
	// 方向刻意反过来：payment/api -> shop/web。种子里那条 allow-api 只管
	// payment/api 的 ingress，管不到这条，因此判定只取决于导入。
	saveIngest(t, s, []flow.Connection{conn(recycledIP, peerIP, portResolved)})

	flows, err := r.Flows(context.Background(), store.FlowFilter{
		Cluster: collectedID, Window: describedWindow(),
	})
	if err != nil {
		t.Fatalf("Flows() = %v", err)
	}
	if len(flows.Items) == 0 {
		t.Fatal("没有流量，这条用例等于没跑")
	}
	for _, f := range flows.Items {
		if f.Verdict == string(replay.VerdictDeny) {
			t.Error("CANDIDATE_ADDITION 的导入改变了当前判定 —— " +
				"那条规则还没下发，平台却把它当成正在生效的")
		}
	}
}
