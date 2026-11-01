package podsupervisor

import "math"

// ID32 converts a user or group id to the unsigned 32-bit form the kernel
// uses. It reports false for a value that does not fit, so that a bad id is an
// error instead of silently becoming another user.
func ID32(v int) (uint32, bool) {
	if v < 0 || int64(v) > math.MaxUint32 {
		return 0, false
	}
	return uint32(v), true
}
