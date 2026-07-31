package replay

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// selectorMatches determines whether a label set is selected by a selector.
//
// It reuses apimachinery's implementation rather than hand-rolling parsing:
// evaluation semantics must match Kubernetes exactly; custom parsing is a
// source of divergence.
//
// nil and empty selectors have different semantics:
//   - NetworkPolicySpec.PodSelector is a value type; an empty object means
//     "select all Pods in the namespace"
//   - NetworkPolicyPeer's PodSelector and NamespaceSelector are pointers;
//     nil means the field is not set
//
// This function returns false for nil, leaving callers to distinguish
// "unset" from "select all".
func selectorMatches(sel *metav1.LabelSelector, lbls map[string]string) (bool, error) {
	if sel == nil {
		return false, nil
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false, fmt.Errorf("convert label selector: %w", err)
	}
	return s.Matches(labels.Set(lbls)), nil
}
