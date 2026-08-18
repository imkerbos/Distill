package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/store"
)

// 一个还没摄入过流量的集群，导出必须能拿得走 —— 但那份文件不得带着
// 一套看起来评估过的 dry-run 数字。
//
// 这份文件**会脱离平台独自存在**：落进一个 PR、贴进工单、隔两天才被应用
// （renderPolicyExport 的注释）。到那时唯一能回答「当初凭什么认为它安全」
// 的只有它自己带的那段话。而零条连接下四类计数全是 0，
// 「dry-run WOULD_BREAK: 0」读起来正好是「应用它不会打断任何东西」。
func TestExportWithoutTrafficDoesNotPrintAnUnearnedZero(t *testing.T) {
	reg := fixtureSource()
	h, _, cookie := newTestRouterWithRegistry(t, noTrafficPreviewReader{}, reg)

	rec := authedGet(t, h, cookie, "/api/v1/clusters/prod-asia-1/policy-export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// 四类计数不得以「算出来了」的形态出现。
	if strings.Contains(body, "dry-run WOULD_BREAK: 0") {
		t.Errorf("the export header printed an unearned zero:\n%s", body)
	}
	// 必须说出为什么没有这几个数。
	if !strings.Contains(body, "没有") || !strings.Contains(body, "流量") {
		t.Errorf("the export header does not say traffic was never observed:\n%s", body)
	}
	// 策略本身要在：拿不走的推荐等于没有推荐。
	if !strings.Contains(body, "kind: NetworkPolicy") {
		t.Errorf("the export carried no policy documents:\n%s", body)
	}
}

// noTrafficPreviewReader 是一个「资产有、流量没有」的 Reader：默认窗口答
// 不出来，但候选策略照常给得出（Baseline 依据资产）。
type noTrafficPreviewReader struct{ store.Reader }

func (noTrafficPreviewReader) DefaultWindow(context.Context, string) (store.TimeWindow, error) {
	return store.TimeWindow{}, collectstore.ErrNoFlowIngest
}

func (noTrafficPreviewReader) PolicyPreviewAtGranularity(
	_ context.Context, clusterID, namespace string, _ store.TimeWindow,
	_ policygen.Granularity,
) (store.PolicyPreview, error) {
	enabled := []networkingv1.NetworkPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "distill-web", Namespace: "shop"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}}
	return store.PolicyPreview{
		Cluster: clusterID, Namespace: namespace,
		TrafficObserved: false,
		Overridden:      store.OverriddenView{Enabled: enabled},
	}, nil
}
