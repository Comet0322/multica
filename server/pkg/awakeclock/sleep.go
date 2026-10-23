package awakeclock

import (
	"math"
	"time"
)

// sleptFromReadings converts a sleep-inclusive tick count (ms) and a
// sleep-exclusive interrupt time (100 ns) into total slept time. A negative
// result comes from the ms tick resolution and is clamped to zero.
func sleptFromReadings(tickMs, unbiased100ns uint64) time.Duration {
	slept := toDuration(tickMs)*time.Millisecond - toDuration(unbiased100ns)*100
	if slept < 0 {
		return 0
	}
	return slept
}

// toDuration converts a counter to a Duration, saturating instead of wrapping.
func toDuration(v uint64) time.Duration {
	if v > math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(v)
}

// sleptDelta returns sleep accrued since origin, never negative.
func sleptDelta(origin, now time.Duration) time.Duration {
	if now < origin {
		return 0
	}
	return now - origin
}
