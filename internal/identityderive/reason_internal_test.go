package identityderive

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// contended 造一个"这个集群另一次推导正在跑"的错误。
func contended() error {
	return fmt.Errorf("cluster prod: %w", snapshotstore.ErrDeriveInProgress)
}

// 推导失败的原因是封闭枚举，且认不出的形态落 OTHER 而不是猜一个。
//
// 原因决定的是处置：LOCK_UNAVAILABLE 的处置是重跑，其余的处置是去查库。
func TestErrorReasonsAreClosed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want snapshotstore.DeriveErrorReason
	}{
		{"a contended cluster", contended(), snapshotstore.DeriveErrorLockUnavailable},
		{"a deadline", context.DeadlineExceeded, snapshotstore.DeriveErrorTimeout},
		{"a wrapped contention", errors.Join(errors.New("gave up"), snapshotstore.ErrDeriveInProgress),
			snapshotstore.DeriveErrorLockUnavailable},
		{"an unrecognised failure", errors.New("something else"), snapshotstore.DeriveErrorOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorReason(tc.err); got != tc.want {
				t.Errorf("errorReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
