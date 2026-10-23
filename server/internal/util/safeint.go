package util

import "math"

// The Sat* helpers narrow an integer by saturating at the destination's
// bounds instead of wrapping, so a hostile or corrupt value cannot turn into a
// negative page size, a huge allocation or a wrong index.

// SatInt32 narrows an int to int32.
func SatInt32(v int) int32 {
	switch {
	case v > math.MaxInt32:
		return math.MaxInt32
	case v < math.MinInt32:
		return math.MinInt32
	}
	return int32(v)
}

// SatInt32From64 narrows an int64 to int32.
func SatInt32From64(v int64) int32 {
	switch {
	case v > math.MaxInt32:
		return math.MaxInt32
	case v < math.MinInt32:
		return math.MinInt32
	}
	return int32(v)
}

// SatInt32FromU64 narrows a uint64 to int32.
func SatInt32FromU64(v uint64) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

// SatUint32 narrows an int to uint32; negative values become 0.
func SatUint32(v int) uint32 {
	switch {
	case v < 0:
		return 0
	case int64(v) > math.MaxUint32:
		return math.MaxUint32
	}
	return uint32(v)
}

// SatUint64 widens an int to uint64; negative values become 0.
func SatUint64(v int) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// SatUint64From64 widens an int64 to uint64; negative values become 0.
func SatUint64From64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// SatInt64FromU64 narrows a uint64 to int64.
func SatInt64FromU64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
