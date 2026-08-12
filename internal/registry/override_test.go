package registry_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
)

func validOverride() registry.RuleOverride {
	return registry.RuleOverride{
		ClusterID: "prod-asia-1", Namespace: "batch", Workload: "worker",
		Fingerprint: strings.Repeat("a", 64),
		Decision:    policygen.DecisionEnable,
		Reason:      "已确认是对账任务，业务侧承诺 Q4 迁走",
		DecidedBy:   "admin", DecidedAt: time.Now().UTC(),
	}
}

func TestValidateOverrideAcceptsAWellFormedOne(t *testing.T) {
	if err := registry.ValidateOverride(validOverride()); err != nil {
		t.Errorf("ValidateOverride() error = %v, want nil", err)
	}
}

// 启用一条已知风险规则必须留下理由。半年后有人问「这条 SSH 出公网
// 为什么是开的」，空理由回答不了。
func TestValidateOverrideRejectsEmptyReason(t *testing.T) {
	o := validOverride()
	o.Reason = "   "
	err := registry.ValidateOverride(o)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("err = %q, want it to name the reason field", err)
	}
}

// 指纹是 SHA-256 的十六进制，长度固定 64。长度不对说明调用方
// 传的不是指纹 —— 放进去会得到一条永远匹配不上的覆盖。
func TestValidateOverrideRejectsMalformedFingerprint(t *testing.T) {
	for name, fp := range map[string]string{
		"tooShort": strings.Repeat("a", 63),
		"tooLong":  strings.Repeat("a", 65),
		"notHex":   strings.Repeat("z", 64),
		"empty":    "",
	} {
		t.Run(name, func(t *testing.T) {
			o := validOverride()
			o.Fingerprint = fp
			if err := registry.ValidateOverride(o); !errors.Is(err, registry.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidateOverrideRejectsUnregisteredDecision(t *testing.T) {
	o := validOverride()
	o.Decision = "SKIP"
	if err := registry.ValidateOverride(o); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestValidateOverrideRequiresIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*registry.RuleOverride){
		"clusterID": func(o *registry.RuleOverride) { o.ClusterID = "" },
		"namespace": func(o *registry.RuleOverride) { o.Namespace = "" },
		"workload":  func(o *registry.RuleOverride) { o.Workload = "" },
	} {
		t.Run(name, func(t *testing.T) {
			o := validOverride()
			mutate(&o)
			err := registry.ValidateOverride(o)
			if !errors.Is(err, registry.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("err = %q, want it to name %s", err, name)
			}
		})
	}
}

// 转换必须逐字段带过去：漏一个字段不会有编译错误，只会让某条
// 人工决定在应用时静默对不上。
func TestToPolicygenCarriesEveryField(t *testing.T) {
	o := validOverride()
	got := o.ToPolicygen()
	if got.Namespace != o.Namespace || got.Workload != o.Workload ||
		got.Fingerprint != o.Fingerprint || got.Decision != o.Decision ||
		got.Reason != o.Reason || got.DecidedBy != o.DecidedBy ||
		!got.DecidedAt.Equal(o.DecidedAt) {
		t.Errorf("ToPolicygen() = %+v, does not match %+v", got, o)
	}
}
