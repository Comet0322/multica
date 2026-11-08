package awakeclock

import (
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// sleptFromReadings converts a sleep-inclusive tick count (ms) and a
// sleep-exclusive interrupt time (100 ns) into total slept time. A negative
// result comes from the ms tick resolution and is clamped to zero.
func sleptFromReadings(tickMs, unbiased100ns uint64) time.Duration {
	slept := time.Duration(util.SatInt64(tickMs))*time.Millisecond - time.Duration(util.SatInt64(unbiased100ns))*100
	if slept < 0 {
		return 0
	}
	return slept
}

// sleptDelta returns sleep accrued since origin, never negative.
func sleptDelta(origin, now time.Duration) time.Duration {
	if now < origin {
		return 0
	}
	return now - origin
}
