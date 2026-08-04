package store

import "testing"

// 一条边聚合多条 flow 时取最严重的 verdict。未登记的取值必须排在最严重
// 一档：新增结论却漏改 verdictSeverity 时，它要被顶到显眼处，
// 而不是永远输给 ALLOW、消失在一片绿里。
func TestMergeVerdictKeepsTheMostSevere(t *testing.T) {
	for _, tc := range []struct {
		cur, next, want string
	}{
		{"", "ALLOW", "ALLOW"},
		{"ALLOW", "DENY", "DENY"},
		{"DENY", "ALLOW", "DENY"},
		{"DENY", "UNKNOWN", "UNKNOWN"},
		{"UNKNOWN", "DENY", "UNKNOWN"},
		{"ALLOW", "QUARANTINED", "QUARANTINED"},
		{"UNKNOWN", "QUARANTINED", "QUARANTINED"},
		{"QUARANTINED", "ALLOW", "QUARANTINED"},
	} {
		if got := mergeVerdict(tc.cur, tc.next); got != tc.want {
			t.Errorf("mergeVerdict(%q, %q) = %q, want %q", tc.cur, tc.next, got, tc.want)
		}
	}
}
