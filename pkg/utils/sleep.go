package utils

import (
	"context"
	"time"
)

// SleepCtx blocks for d, or until ctx is done, whichever comes first. When d
// is not positive it returns immediately without checking ctx. Returns nil
// after a full sleep, or ctx.Err() if ctx was done first — so a caller that
// only cares whether the sleep was cut short can just check the error is
// non-nil, and one that wants the reason still has it.
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
