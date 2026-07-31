package replay

import "testing"

// 多个未决原因并存时的归类必须由固定优先级决定，不能由遍历顺序决定：
// §17 要统计 UNKNOWN 的构成来定位"该修哪个子系统"，后写覆盖会让同一份
// 输入换个排列就换一个分类，这个指标随之不可复现。
func TestEscalatePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		cur, next UnknownReason
		want      UnknownReason
	}{
		{"none stays none", ReasonNone, ReasonNone, ReasonNone},
		{"any reason beats none", ReasonNone, ReasonNamedPortUnresolved, ReasonNamedPortUnresolved},
		{"none never overwrites", ReasonSnapshotMissing, ReasonNone, ReasonSnapshotMissing},
		{"snapshot beats named port", ReasonNamedPortUnresolved, ReasonSnapshotMissing, ReasonSnapshotMissing},
		{"named port does not demote snapshot", ReasonSnapshotMissing, ReasonNamedPortUnresolved, ReasonSnapshotMissing},
		{"malformed beats snapshot", ReasonSnapshotMissing, ReasonPolicyMalformed, ReasonPolicyMalformed},
		{"snapshot does not demote malformed", ReasonPolicyMalformed, ReasonSnapshotMissing, ReasonPolicyMalformed},
		{"malformed beats named port", ReasonNamedPortUnresolved, ReasonPolicyMalformed, ReasonPolicyMalformed},
		{"equal keeps current", ReasonSnapshotMissing, ReasonSnapshotMissing, ReasonSnapshotMissing},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escalate(tt.cur, tt.next); got != tt.want {
				t.Errorf("escalate(%q, %q) = %q, want %q", tt.cur, tt.next, got, tt.want)
			}
		})
	}
}

// escalate 必须是交换律成立的：调换两个原因的先后不改变结果。这正是
// "遍历顺序不得影响分类"的形式化表述。
func TestEscalateIsOrderIndependent(t *testing.T) {
	all := []UnknownReason{
		ReasonNone, ReasonNamedPortUnresolved, ReasonSnapshotMissing, ReasonPolicyMalformed,
	}
	for _, a := range all {
		for _, b := range all {
			if escalate(a, b) != escalate(b, a) {
				t.Errorf("escalate(%q,%q)=%q but escalate(%q,%q)=%q; order must not matter",
					a, b, escalate(a, b), b, a, escalate(b, a))
			}
		}
	}
}
