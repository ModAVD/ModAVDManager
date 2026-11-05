package emu

import (
	"context"
	"fmt"
	"math"
	"time"

	"modavd/internal/errs"
)

// Zero means an unknown work size: retain cancellation without a deadline.
func qemuImgContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}

func qemuImgTimeoutText(budget time.Duration) string {
	if budget >= time.Hour {
		hours, minutes := budget/time.Hour, budget%time.Hour/time.Minute
		if minutes > 0 {
			return fmt.Sprintf("%d 小时 %d 分钟", hours, minutes)
		}
		return fmt.Sprintf("%d 小时", hours)
	}
	if budget >= time.Minute {
		return fmt.Sprintf("%d 分钟", budget/time.Minute)
	}
	if budget >= time.Second {
		return fmt.Sprintf("%d 秒", budget/time.Second)
	}
	return budget.String()
}

// compactTimeout allows 1 MiB/s of aggregate I/O, plus five minutes for setup.
// Convert may read both images and write a new overlay (three passes); compare
// may read both complete backing chains (two passes). Use the virtual size as
// well as live allocation: a small overlay can inherit a large amount of data.
// These are conservative budgets, not speed predictions. Unknown sizes have no
// automatic deadline; the caller's context still cancels the operation.
func compactTimeout(virtual, allocated, onDisk int64, passes int64, floor time.Duration) time.Duration {
	work := max(virtual, allocated, onDisk)
	if virtual <= 0 || work <= 0 {
		return 0
	}
	const rate = int64(1 << 20)
	seconds := work / rate
	if work%rate != 0 {
		seconds++
	}
	const setup = int64(5 * 60)
	const maxSeconds = math.MaxInt64 / int64(time.Second)
	if seconds > (maxSeconds-setup)/passes {
		return time.Duration(math.MaxInt64)
	}
	return max(floor, time.Duration(seconds*passes+setup)*time.Second)
}

// The estimate is deliberately optimistic: it omits new qcow2 metadata and
// cannot discover zero-filled live clusters without reading them. Avoid a full
// rewrite when even this estimate saves less than 16 MiB or 1% of host usage.
// Equality qualifies for rewriting. Missing allocation never qualifies to skip.
func compactBenefit(before int64, allocated *int64) (estimate *int64, threshold int64) {
	threshold = max(16<<20, before/100)
	if allocated != nil {
		n := reclaimable(before, *allocated)
		estimate = &n
	}
	return
}

// checkedCompactAllocation validates external qemu-img counts before arithmetic.
func checkedCompactAllocation(clusters, clusterBytes int64) (int64, error) {
	if clusters < 0 || clusterBytes <= 0 || clusters > math.MaxInt64/clusterBytes {
		return 0, errs.New(errs.Internal, "qemu-img check 的已分配大小不合理，无法整理")
	}
	return clusters * clusterBytes, nil
}

func compactRequiredSpace(allocated int64) (int64, error) {
	if allocated > math.MaxInt64-allocated/20-compactSpaceSlack {
		return 0, errs.New(errs.Unsupported, "整理所需空间超出可表示范围，无法整理")
	}
	return allocated + allocated/20 + compactSpaceSlack, nil
}

func compactResult(i int, overlay string, before, after int64, estimate *int64, threshold int64, skipped bool) *DiskCompactResult {
	return &DiskCompactResult{
		Index: i, Overlay: overlay, BeforeOnDiskBytes: before, AfterOnDiskBytes: after,
		FreedBytes: reclaimable(before, after), OnDiskDeltaBytes: after - before,
		Skipped: skipped, EstimatedReclaimableBytes: estimate, ReclaimThresholdBytes: threshold,
	}
}
