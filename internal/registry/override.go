package registry

import (
	"fmt"
	"strings"
	"time"

	"github.com/imkerbos/Distill/internal/policygen"
)

// fingerprintLen 是 SHA-256 十六进制表示的长度。
const fingerprintLen = 64

// RuleOverride 是一条落库的人工决定。
//
// 与 policygen.Override 分开：这个类型带着持久化才需要的字段
// （集群、合并 commit、软删除），而 policygen 是纯逻辑层，
// 不该知道这些东西存在。
type RuleOverride struct {
	// ClusterID 是所属集群，主键首列。
	ClusterID string `json:"clusterId"`
	// Namespace 与 Workload 定位到一条候选策略。
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	// Fingerprint 是被覆盖规则的内容指纹。
	Fingerprint string `json:"fingerprint"`
	// Decision 是决定方向。复用 policygen 的枚举而非另起一套 ——
	// 两个同值枚举并存迟早漂移。
	Decision policygen.OverrideDecision `json:"decision"`
	// Reason 是理由，非空。
	Reason string `json:"reason"`
	// DecidedBy 与 DecidedAt 是溯源信息。
	DecidedBy string    `json:"decidedBy"`
	DecidedAt time.Time `json:"decidedAt"`
	// MergedCommitSHA 是这条决定落进 Git 的 commit；空表示尚未落地。
	//
	// 「我点了启用」与「它已经在集群里生效」之间隔着一次人工 review
	// 和一个 Config Sync 周期，界面必须把两者分开显示。本轮恒为空。
	MergedCommitSHA string `json:"mergedCommitSha"`
}

// ToPolicygen 转成纯逻辑层的形态。
func (o RuleOverride) ToPolicygen() policygen.Override {
	return policygen.Override{
		Namespace: o.Namespace, Workload: o.Workload,
		Fingerprint: o.Fingerprint, Decision: o.Decision,
		Reason: o.Reason, DecidedBy: o.DecidedBy, DecidedAt: o.DecidedAt,
	}
}

// ValidateOverride 校验一条人工决定。
//
// 身份三项按固定顺序逐个查，而不是 range 一个 map：这段文案经
// WriteInvalid 直接回给调用方，两个字段同时为空时 map 的遍历顺序会让
// 两次一模一样的错误请求得到不同的字段名 —— 排查的人据此改一个字段，
// 重试又被另一个字段拒绝，看起来像服务在随机拒绝他。ValidateCluster
// 用的就是 if 链，这里对齐。
func ValidateOverride(o RuleOverride) error {
	for _, f := range []struct {
		name  string
		value string
	}{
		{"clusterID", o.ClusterID},
		{"namespace", o.Namespace},
		{"workload", o.Workload},
	} {
		if f.value == "" {
			return NewInvalidError(fmt.Sprintf("%s is required", f.name))
		}
	}
	if !o.Decision.Valid() {
		return NewInvalidError(fmt.Sprintf("unregistered decision %q", o.Decision))
	}
	// 理由 trim 后为空即拒绝：一串空格能通过 NOT NULL，但回答不了
	// 「这条规则为什么是开的」。
	if strings.TrimSpace(o.Reason) == "" {
		return NewInvalidError("reason is required")
	}
	if err := checkFingerprint(o.Fingerprint); err != nil {
		return err
	}
	return nil
}

// checkFingerprint 校验指纹形状。
//
// 长度与字符集都查：调用方传来的若不是指纹，写进去会得到一条
// 永远匹配不上的覆盖 —— 它不报错，只会永远待在「已失效」那一节，
// 而它从来就没生效过。
func checkFingerprint(fp string) error {
	if len(fp) != fingerprintLen {
		return NewInvalidError(fmt.Sprintf(
			"fingerprint must be %d hex characters, got %d", fingerprintLen, len(fp)))
	}
	for _, c := range fp {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			return NewInvalidError("fingerprint must be lowercase hexadecimal")
		}
	}
	return nil
}
