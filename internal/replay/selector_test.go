package replay

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSelectorMatches(t *testing.T) {
	labels := map[string]string{"app": "api", "tier": "backend"}

	tests := []struct {
		name string
		sel  *metav1.LabelSelector
		want bool
	}{
		{
			name: "empty selector selects everything",
			sel:  &metav1.LabelSelector{},
			want: true,
		},
		{
			name: "matching matchLabels",
			sel:  &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			want: true,
		},
		{
			name: "non-matching matchLabels",
			sel:  &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			want: false,
		},
		{
			name: "all matchLabels must match",
			sel:  &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api", "tier": "frontend"}},
			want: false,
		},
		{
			name: "matchExpressions In",
			sel: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"api", "web"}},
			}},
			want: true,
		},
		{
			name: "matchExpressions NotIn",
			sel: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"api"}},
			}},
			want: false,
		},
		{
			name: "matchExpressions Exists",
			sel: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "tier", Operator: metav1.LabelSelectorOpExists},
			}},
			want: true,
		},
		{
			name: "matchExpressions DoesNotExist",
			sel: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "absent", Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
			want: true,
		},
		{
			name: "matchLabels and matchExpressions are ANDed",
			sel: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "api"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"frontend"}},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectorMatches(tt.sel, labels)
			if err != nil {
				t.Fatalf("selectorMatches: unexpected error %v", err)
			}
			if got != tt.want {
				t.Errorf("selectorMatches = %v, want %v", got, tt.want)
			}
		})
	}
}

// nil 与空 selector 语义不同：nil 表示字段未设置，调用方需自行处理；
// 空对象表示"选中全部"。混淆二者会让 peer 匹配范围失控。
func TestSelectorMatchesNilSelectsNothing(t *testing.T) {
	got, err := selectorMatches(nil, map[string]string{"app": "api"})
	if err != nil {
		t.Fatalf("selectorMatches: %v", err)
	}
	if got {
		t.Error("nil selector must not match; callers distinguish unset from empty")
	}
}

func TestSelectorMatchesRejectsInvalidOperator(t *testing.T) {
	sel := &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "app", Operator: "Bogus", Values: []string{"x"}},
	}}
	if _, err := selectorMatches(sel, nil); err == nil {
		t.Fatal("selectorMatches returned nil error for an invalid operator")
	}
}
