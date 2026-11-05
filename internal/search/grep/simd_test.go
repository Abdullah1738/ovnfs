//go:build goexperiment.simd

package grep

import (
	"bytes"
	"testing"
)

// The vector searches must agree with the byte loops for every buffer
// length, start, and hit position across the 4-vector, 1-vector and tail
// loops (vectors are up to 64 bytes).
func TestVectorSearchesAgree(t *testing.T) {
	st := &dstate{ranges: []byteRange{{'0', 9}, {'x', 0}, {0x80, 0x7f}}, exits: new([256]bool)}
	for _, r := range st.ranges {
		for c := int(r.lo); c <= int(r.lo)+int(r.span); c++ {
			st.exits[c] = true
		}
	}
	for n := range 300 {
		for hit := -1; hit < n; hit++ {
			buf := bytes.Repeat([]byte("a-"), n)[:n]
			if hit >= 0 {
				buf[hit] = "7x\xc3\n"[hit%4]
			}
			for _, from := range []int{0, 1, n / 2} {
				if from > n {
					continue
				}
				if got, want := findExit(st, buf, from), findExitBytes(st.exits, buf, from); got != want {
					t.Fatalf("findExit(len %d, hit %d, from %d) = %d, want %d", n, hit, from, got, want)
				}
			}
			if got, want := lastNewline(buf), bytes.LastIndexByte(buf, '\n'); got != want {
				t.Fatalf("lastNewline(len %d, hit %d) = %d, want %d", n, hit, got, want)
			}
		}
	}

	// Literal candidates: a one-byte probe, a probe pair, and a fold pair
	// (both cases of each letter must pass).
	c, err := compilePattern(`@|Ab|(?i)qz`, false, false, false)
	if err != nil || c.pre == nil || len(c.pre.members) != 3 {
		t.Fatalf("prefilter of @|Ab|(?i)qz: %v", err)
	}
	for _, m := range c.pre.members {
		for n := range 300 {
			for hit := -1; hit+len(m.lit) <= n; hit++ {
				buf := bytes.Repeat([]byte("a-"), n)[:n]
				if hit >= 0 {
					copy(buf[hit:], m.lit)
					if m.fold && hit%2 == 1 {
						buf[hit] = upper(buf[hit])
					}
				}
				for _, from := range []int{0, 1, n / 2} {
					var pc probeCache
					got, want := m.candidate(buf, from, &pc), m.candidateBytes(buf, from, n-len(m.lit)+1)
					if got != want {
						t.Fatalf("%q candidate(len %d, hit %d, from %d) = %d, want %d", m.lit, n, hit, from, got, want)
					}
				}
			}
		}
	}
}
