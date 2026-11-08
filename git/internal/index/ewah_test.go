package index

import (
	"runtime"
	"testing"
)

func TestDecodeEWAHBoundsBitCount(t *testing.T) {
	huge := []byte{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, _, err := decodeEWAH(huge, 100); err == nil {
		t.Fatal("bitmap of 2^32-1 bits accepted for 100 items")
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("%d bytes allocated rejecting an oversized bitmap", allocated)
	}
	// A run of ones longer than the declared bits is clamped, not expanded.
	run := []byte{0, 0, 0, 10, 0, 0, 0, 1, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0}
	bm, rest, err := decodeEWAH(run, 10)
	if err != nil || len(rest) != 0 || len(bm.words) != 1 {
		t.Fatalf("bitmap %+v, rest %d, err %v", bm, len(rest), err)
	}
	n := 0
	for range bm.ones() {
		n++
	}
	if n != 10 {
		t.Fatalf("%d bits set, want 10", n)
	}
}
