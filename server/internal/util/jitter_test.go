package util

import "testing"

func TestRandInt63nStaysInRange(t *testing.T) {
	for _, n := range []int64{1, 2, 7, 1000, 1 << 40} {
		for i := 0; i < 200; i++ {
			if v := RandInt63n(n); v < 0 || v >= n {
				t.Fatalf("RandInt63n(%d) = %d, out of range", n, v)
			}
		}
	}
	if RandInt63n(0) != 0 || RandInt63n(-5) != 0 {
		t.Fatal("non-positive bound must return 0")
	}
}

func TestRandFloat64StaysInRange(t *testing.T) {
	for i := 0; i < 500; i++ {
		if v := RandFloat64(); v < 0 || v >= 1 {
			t.Fatalf("RandFloat64() = %v, out of range", v)
		}
	}
}

func TestSatNarrowingSaturates(t *testing.T) {
	if SatInt32(1<<40) != 2147483647 || SatInt32(-1<<40) != -2147483648 || SatInt32(7) != 7 {
		t.Fatal("SatInt32")
	}
	if SatInt32From64(1<<40) != 2147483647 || SatInt32From64(-5) != -5 {
		t.Fatal("SatInt32From64")
	}
	if SatInt32FromU64(1<<40) != 2147483647 || SatInt32FromU64(9) != 9 {
		t.Fatal("SatInt32FromU64")
	}
	if SatUint32(-1) != 0 || SatUint32(1<<40) != 4294967295 || SatUint32(3) != 3 {
		t.Fatal("SatUint32")
	}
	if SatUint64(-1) != 0 || SatUint64(3) != 3 || SatUint64From64(-2) != 0 {
		t.Fatal("SatUint64")
	}
	if SatInt64FromU64(1<<63) != 1<<63-1 || SatInt64FromU64(4) != 4 {
		t.Fatal("SatInt64FromU64")
	}
}
