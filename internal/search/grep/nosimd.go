//go:build !goexperiment.simd

package grep

import "bytes"

// The fallbacks of simd.go for builds without GOEXPERIMENT=simd: literal
// candidates come from bytes.IndexByte on the rarest probe byte, newlines
// from bytes.LastIndexByte, and accelerated DFA skips loop over bytes.

// probeCache holds the next occurrence of each case of a member's probe
// byte (unknown: not searched yet, -1: none), so scanning with increasing
// positions never rescans a byte.
type probeCache [2]int

func (c *probeCache) reset() { *c = probeCache{unknown, unknown} }

// extend forgets misses from a bounded search, retaining actual positions.
func (c *probeCache) extend() {
	for i, pos := range c {
		if pos == -1 {
			c[i] = unknown
		}
	}
}

// candidate returns the first start s ≥ pos of a possible occurrence of
// m's literal (either case of its first probe byte matches and the literal
// fits in buf), or -1.
func (m *member) candidate(buf []byte, pos int, pc *probeCache) int {
	variants := 2
	if m.cases[0] == m.cases[1] {
		variants = 1
	}
	pos += m.at
	q := -1
	for v := range variants {
		o := pc[v]
		if o != -1 && o < pos {
			o = -1
			if pos < len(buf) {
				if i := bytes.IndexByte(buf[pos:], m.cases[v]); i >= 0 {
					o = pos + i
				}
			}
			pc[v] = o
		}
		if o >= 0 && (q < 0 || o < q) {
			q = o
		}
	}
	if q < 0 || q-m.at+len(m.lit) > len(buf) {
		return -1
	}
	return q - m.at
}

// findExit returns the first index at or after from whose byte may leave
// accelerated state st, or len(buf).
func findExit(st *dstate, buf []byte, from int) int {
	return findExitBytes(st.exits, buf, from)
}

// lastNewline returns the index of the last '\n' in s, or -1.
func lastNewline(s []byte) int { return bytes.LastIndexByte(s, '\n') }
