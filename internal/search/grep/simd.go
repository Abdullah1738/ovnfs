//go:build goexperiment.simd

package grep

import (
	"bytes"
	"math/bits"
	"simd"
)

// The package's vector searches, on the portable vectors of package simd
// (builds with GOEXPERIMENT=simd; nosimd.go has the fallbacks): literal
// candidates, accelerated DFA skips, and newline searches. Vector width (16
// to 64 bytes) is fixed per process. simd has no movemask or horizontal
// reduction, so a lane mask is stored as 64-bit words and searched with bit
// scans. With emulated vectors (no hardware support) the byte loops are
// faster, and so they are for short stretches, below bytewise bytes.
//
// Each search is a plain function that handles short stretches and
// emulation, and a *Vector function holding the vector code: the compiler
// dispatches every call of a function using vectors on the vector width,
// which short stretches should not pay for. The *Vector functions are not
// methods because methods using vectors crash the go1.27.2 compiler
// ("missing Types entry").

// maxVectorWords is the 64-bit words of the widest vector (AVX-512).
const maxVectorWords = 8

// bytewise is the shortest stretch the candidate and newline searches
// search with vectors: below it the setup costs more than it saves
// (measured on an Apple M4 Max, 16-byte vectors). DFA skips check their
// first skipHead bytes one at a time before calling findExit.
const bytewise = 64

// A member's first probe byte is dense in a buffer once, over at least
// denseProbes false hits (the second probe fails), they average fewer than
// denseGap bytes apart: past that, the cost of a false hit of
// bytes.IndexByte (about 4ns, against vectors' 0.06ns per byte, Apple M4
// Max) makes scanCandidates the faster search. True hits cost a call
// either way.
const (
	denseProbes = 32
	denseGap    = 64
)

// probeCache tracks a member's first probe byte in the buffer: the false
// hits bytes.IndexByte found, the bytes searched, and whether that made
// the byte dense.
type probeCache struct {
	misses, span int
	dense        bool
}

func (c *probeCache) reset() { *c = probeCache{} }

// Vector probes cache density, not misses, across bounded searches.
func (c *probeCache) extend() {}

// candidate returns the first start s ≥ pos of a possible occurrence of
// m's literal (both probe bytes match and the literal fits in buf), or -1.
// A first probe byte with one case is searched with bytes.IndexByte, whose
// assembly outruns scanCandidates while the byte is rare, and the second
// probe checked at each hit; once the byte proves dense, and for fold
// letters, scanCandidates tests both probes for blocks of starts.
func (m *member) candidate(buf []byte, pos int, pc *probeCache) int {
	end := len(buf) - len(m.lit) + 1 // starts below end fit the literal
	for m.cases[0] == m.cases[1] && !pc.dense {
		if pos >= end {
			return -1
		}
		i := bytes.IndexByte(buf[pos+m.at:end+m.at], m.cases[0])
		if i < 0 {
			return -1
		}
		pc.span += i + 1
		if s := pos + i; buf[s+m.at2]|m.mask[1] == m.want[1] {
			return s
		}
		pc.misses++
		pc.dense = pc.misses >= denseProbes && pc.span < pc.misses*denseGap
		pos += i + 1
	}
	if end-pos < bytewise || simd.Emulated() {
		return m.candidateBytes(buf, pos, end)
	}
	return candidateVector(m, buf, pos, end)
}

// candidateVector returns the first start in [pos, end) passing both of
// m's probes, or -1. It tests a block of starts at once (lane k of
// (v|mask)^want is 0 exactly when start k passes a probe), four vectors per
// horizontal check.
func candidateVector(m *member, buf []byte, pos, end int) int {
	mask0, want0 := simd.BroadcastUint8s(m.mask[0]), simd.BroadcastUint8s(m.want[0])
	mask1, want1 := simd.BroadcastUint8s(m.mask[1]), simd.BroadcastUint8s(m.want[1])
	pair := m.at2 != m.at
	p0, p1 := buf[m.at:], buf[m.at2:]
	// miss is 0 in the lanes of the starts at s that pass both probes.
	miss := func(s int) simd.Uint8s {
		z := simd.LoadUint8s(p0[s:]).Or(mask0).Xor(want0)
		if pair {
			z = z.Or(simd.LoadUint8s(p1[s:]).Or(mask1).Xor(want1))
		}
		return z
	}
	var words [maxVectorWords]uint64
	n := simd.Uint8s{}.Len()
	s := pos
	for ; s+4*n <= end; s += 4 * n {
		if firstZero(miss(s).Min(miss(s+n)).Min(miss(s+2*n)).Min(miss(s+3*n)), &words) >= 0 {
			break
		}
	}
	for ; s+n <= end; s += n {
		if k := firstZero(miss(s), &words); k >= 0 {
			return s + k
		}
	}
	return m.candidateBytes(buf, s, end)
}

// findExit returns the first index at or after from whose byte may leave
// accelerated state st, or len(buf).
func findExit(st *dstate, buf []byte, from int) int {
	if simd.Emulated() {
		return findExitBytes(st.exits, buf, from)
	}
	return findExitVector(st, buf, from)
}

// findExitVector is findExit with vectors. A byte c is in an exit range
// [lo, lo+span] exactly when (c - lo) saturating-minus span is 0, so a
// vector holds an exit when the lane-wise minimum over the ranges has a
// zero lane. Blocks of four vectors share one horizontal check.
func findExitVector(st *dstate, buf []byte, from int) int {
	ranges := st.ranges
	var los, spans [maxAccelRanges]simd.Uint8s
	for k, r := range ranges {
		los[k] = simd.BroadcastUint8s(r.lo)
		spans[k] = simd.BroadcastUint8s(r.span)
	}
	// dist is 0 in the lanes of v that hold an exit.
	dist := func(v simd.Uint8s) simd.Uint8s {
		z := v.Sub(los[0]).SubSaturated(spans[0])
		for k := 1; k < len(ranges); k++ {
			z = z.Min(v.Sub(los[k]).SubSaturated(spans[k]))
		}
		return z
	}
	var words [maxVectorWords]uint64
	n := simd.Uint8s{}.Len()
	i := from
	for ; i+4*n <= len(buf); i += 4 * n {
		z := dist(simd.LoadUint8s(buf[i:])).Min(dist(simd.LoadUint8s(buf[i+n:]))).
			Min(dist(simd.LoadUint8s(buf[i+2*n:]))).Min(dist(simd.LoadUint8s(buf[i+3*n:])))
		if firstZero(z, &words) >= 0 {
			break
		}
	}
	for ; i+n <= len(buf); i += n {
		if k := firstZero(dist(simd.LoadUint8s(buf[i:])), &words); k >= 0 {
			return i + k
		}
	}
	return findExitBytes(st.exits, buf, i)
}

// firstZero returns the index of v's first zero lane, or -1; words is
// scratch.
func firstZero(v simd.Uint8s, words *[maxVectorWords]uint64) int {
	v.Equal(simd.Uint8s{}).ToInt8s().ToBits().ReshapeToUint64s().Store(words[:])
	for w, m := range words[:v.Len()/8] {
		if m != 0 {
			return w*8 + bits.TrailingZeros64(m)/8
		}
	}
	return -1
}

// lastNewline returns the index of the last '\n' in s, or -1.
func lastNewline(s []byte) int {
	if len(s) < bytewise || simd.Emulated() {
		return bytes.LastIndexByte(s, '\n')
	}
	return lastNewlineVector(s)
}

// lastNewlineVector is lastNewline scanning backwards a vector at a time.
func lastNewlineVector(s []byte) int {
	nl := simd.BroadcastUint8s('\n')
	n := nl.Len()
	var words [maxVectorWords]uint64
	i := len(s)
	for i >= n {
		i -= n
		simd.LoadUint8s(s[i:]).Equal(nl).ToInt8s().ToBits().ReshapeToUint64s().Store(words[:])
		for w := n/8 - 1; w >= 0; w-- {
			if m := words[w]; m != 0 {
				return i + w*8 + 7 - bits.LeadingZeros64(m)/8
			}
		}
	}
	return bytes.LastIndexByte(s[:i], '\n')
}
