package util

import (
	"crypto/rand"
	"math/big"
)

// RandInt63n returns a uniform value in [0, n). It returns 0 when n <= 0 or
// the system entropy source fails, which makes a jitter collapse to its base
// delay rather than fail the caller.
func RandInt63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

// RandFloat64 returns a uniform value in [0, 1).
func RandFloat64() float64 {
	const span = 1 << 53
	return float64(RandInt63n(span)) / span
}
