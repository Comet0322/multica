package util

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"time"
)

type integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// SatInt32 converts v to int32, clamping values outside the int32 range.
func SatInt32[T integer](v T) int32 {
	if v < 0 {
		if int64(v) < math.MinInt32 {
			return math.MinInt32
		}
		return int32(v)
	}
	if uint64(v) > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

// RandInt63n returns a uniformly distributed value in [0, n) from crypto/rand.
// It returns 0 when n <= 0 or the system source fails.
func RandInt63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return SatInt64(binary.BigEndian.Uint64(b[:]) % SatUint64(n))
}

// RandFloat64 returns a value in [0, 1) from crypto/rand.
func RandFloat64() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}

// RandDuration returns a duration in [0, d).
func RandDuration(d time.Duration) time.Duration {
	return time.Duration(RandInt63n(int64(d)))
}

// SatUint64 converts v to uint64, clamping negative values to 0.
func SatUint64[T integer](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// SatUint32 converts v to uint32, clamping values outside the uint32 range.
func SatUint32[T integer](v T) uint32 {
	if v < 0 {
		return 0
	}
	if uint64(v) > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

// SatInt64 converts v to int64, clamping values above the int64 range.
func SatInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// LowByte returns the least-significant byte of v.
func LowByte(v uint64) byte {
	return byte(v & 0xff)
}
