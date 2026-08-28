package collectstore

import (
	"testing"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// Service 这次采集完全没枚举过时，EXPOSED_INGRESS 必须落进未评估清单。
//
// 少了这条注册，exposedByLBOrNodePortService 会把"没采到 Service"读成
// "这个 namespace 没有暴露对象"，applicability 判它不适用，从 Missing()
// 里悄悄消失——一次采集失败（403/超时）就此变成一次放行
// （design review C2，2026-08-28）。
func TestNotAssessedBaselinesIncludesExposedIngressWhenServiceNeverEnumerated(t *testing.T) {
	e := runEvidence{
		enumerated: map[snapshot.ResourceKind]bool{},
		failed:     map[snapshot.ResourceKind]bool{},
	}
	var found bool
	for _, k := range notAssessedBaselines(e) {
		if k == baseline.KindExposedIngress {
			found = true
		}
	}
	if !found {
		t.Errorf("Service 完全没枚举过时 EXPOSED_INGRESS 没有落进未评估清单: %v",
			notAssessedBaselines(e))
	}
}

// Service 枚举了但那一类失败了（403/超时），同样要落进未评估——
// "有计数行"不等于"拿回来了"，cameBack 的第二种情形。
func TestNotAssessedBaselinesIncludesExposedIngressWhenServiceFailed(t *testing.T) {
	e := runEvidence{
		enumerated: map[snapshot.ResourceKind]bool{snapshot.ResourceService: true},
		failed:     map[snapshot.ResourceKind]bool{snapshot.ResourceService: true},
	}
	var found bool
	for _, k := range notAssessedBaselines(e) {
		if k == baseline.KindExposedIngress {
			found = true
		}
	}
	if !found {
		t.Errorf("Service 采集失败时 EXPOSED_INGRESS 没有落进未评估清单: %v",
			notAssessedBaselines(e))
	}
}

// Service 真的拿回来了，EXPOSED_INGRESS 就是可评估的，不该出现在未评估
// 清单里——那会让一个采集齐备的集群也带着一份"我们没看过"的标注。
func TestNotAssessedBaselinesExcludesExposedIngressWhenServiceCameBack(t *testing.T) {
	e := runEvidence{
		enumerated: map[snapshot.ResourceKind]bool{snapshot.ResourceService: true},
		failed:     map[snapshot.ResourceKind]bool{},
	}
	for _, k := range notAssessedBaselines(e) {
		if k == baseline.KindExposedIngress {
			t.Errorf("Service 采到了，EXPOSED_INGRESS 却仍在未评估清单里: %v",
				notAssessedBaselines(e))
		}
	}
}
