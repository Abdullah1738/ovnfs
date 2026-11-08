package merge

// hunk is one change between two line sequences: lines a[A:A+NA] are replaced
// by b[B:B+NB]. Hunks are ordered and never touch: at least one equal line
// separates consecutive hunks.
type hunk struct {
	A, NA, B, NB int
}

// diffLines computes the changes from a to b, lines given as interned ids.
//
// The edit script comes from Myers' linear-space middle-snake bisection, run
// only over the lines worth matching (see reduce) and cut short on very
// different inputs (see bestReach); each change group is then slid to the
// position git reports: as low as possible, unless an earlier position lines
// it up with a change in the other sequence. The three-way result depends on
// reproducing git's alignment exactly, and every one of these steps affects
// it; their details were established against git's own output.
func diffLines(a, b []int32) []hunk {
	ca := make([]bool, len(a))
	cb := make([]bool, len(b))

	// Lines common to both ends never take part in the search.
	lo, hiA, hiB := 0, len(a), len(b)
	for lo < hiA && lo < hiB && a[lo] == b[lo] {
		lo++
	}
	for hiA > lo && hiB > lo && a[hiA-1] == b[hiB-1] {
		hiA--
		hiB--
	}
	ra, ia := reduce(a, b, lo, hiA, ca)
	rb, ib := reduce(b, a, lo, hiB, cb)
	d := differ{a: ra, b: rb, ca: make([]bool, len(ra)), cb: make([]bool, len(rb))}
	d.maxCost = max(minCost, bogoSqrt(len(ra)+len(rb)+3))
	n := len(ra) + len(rb) + 3
	d.fwd = make([]int, 2*n)
	d.bwd = make([]int, 2*n)
	d.off = n
	d.compare(0, len(ra), 0, len(rb))
	for i, c := range d.ca {
		ca[ia[i]] = c
	}
	for i, c := range d.cb {
		cb[ib[i]] = c
	}
	compact(a, ca, cb)
	compact(b, cb, ca)
	return hunks(ca, cb)
}

// Line classes used when deciding which lines can take part in the search.
const (
	keepLine    = iota // occurs in the other file a reasonable number of times
	discardLine        // does not occur in the other file at all
	commonLine         // occurs in the other file very often
)

// reduce returns the lines of x[lo:hi] worth searching for in y, with their
// indices in x, and marks the others as changed in cx. Lines y lacks cannot
// match anyway; lines y has many copies of are dropped too when they sit among
// such lines, which keeps the search from aligning on noise. The thresholds
// were determined by experiment against git's alignments.
func reduce(x, y []int32, lo, hi int, cx []bool) ([]int32, []int) {
	count := make(map[int32]int, len(y))
	for _, id := range y {
		count[id]++
	}
	lim := bogoSqrt(len(x))
	class := make([]byte, hi-lo)
	for i := lo; i < hi; i++ {
		switch nm := count[x[i]]; {
		case nm == 0:
			class[i-lo] = discardLine
		case nm >= lim:
			class[i-lo] = commonLine
		}
	}
	ids := make([]int32, 0, hi-lo)
	idx := make([]int, 0, hi-lo)
	for i := lo; i < hi; i++ {
		c := class[i-lo]
		if c == keepLine || c == commonLine && !dropCommon(class, i-lo) {
			ids = append(ids, x[i])
			idx = append(idx, i)
		} else {
			cx[i] = true
		}
	}
	return ids, idx
}

// scanWindow bounds how far dropCommon looks to each side of a line.
const scanWindow = 100

// dropCommon decides whether the common line class[i] is dropped: only when
// its run of non-kept lines has discarded lines on both sides of it, and they
// outnumber the common lines (the line itself weighing double) three to one.
func dropCommon(class []byte, i int) bool {
	dis, com := 0, 2
	for _, step := range []int{-1, 1} {
		d := 0
		for k, n := i+step, 0; k >= 0 && k < len(class) && n < scanWindow; k, n = k+step, n+1 {
			if class[k] == keepLine {
				break
			}
			if class[k] == discardLine {
				d++
			} else {
				com++
			}
		}
		if d == 0 {
			return false
		}
		dis += d
	}
	return com*4 < com+dis
}

// bogoSqrt is a cheap power-of-two estimate of the square root.
func bogoSqrt(n int) int {
	i := 1
	for ; n > 0; n >>= 2 {
		i <<= 1
	}
	return i
}

// differ holds the inputs, the per-line change marks being computed, and the
// furthest-reaching vectors shared by every bisection step.
type differ struct {
	a, b     []int32
	ca, cb   []bool
	fwd, bwd []int // indexed by diagonal k = i - j, shifted by off
	off      int
	maxCost  int
}

// compare marks the changed lines of a[lo1:hi1] against b[lo2:hi2].
func (d *differ) compare(lo1, hi1, lo2, hi2 int) {
	for {
		for lo1 < hi1 && lo2 < hi2 && d.a[lo1] == d.b[lo2] {
			lo1++
			lo2++
		}
		for lo1 < hi1 && lo2 < hi2 && d.a[hi1-1] == d.b[hi2-1] {
			hi1--
			hi2--
		}
		switch {
		case lo1 == hi1:
			for j := lo2; j < hi2; j++ {
				d.cb[j] = true
			}
			return
		case lo2 == hi2:
			for i := lo1; i < hi1; i++ {
				d.ca[i] = true
			}
			return
		}
		i, j := d.split(lo1, hi1, lo2, hi2)
		d.compare(lo1, i, lo2, j)
		lo1, lo2 = i, j
	}
}

// split finds a point (i, j) on an optimal edit path through the box
// a[lo1:hi1] × b[lo2:hi2] by running the forward and backward searches until
// they overlap. Both ends of the box differ, so the point is never a corner.
func (d *differ) split(lo1, hi1, lo2, hi2 int) (int, int) {
	a, b, fwd, bwd, off := d.a, d.b, d.fwd, d.bwd, d.off
	kmin, kmax := lo1-hi2, hi1-lo2
	fmid, bmid := lo1-lo2, hi1-hi2
	odd := (fmid-bmid)&1 != 0
	fmin, fmax, bmin, bmax := fmid, fmid, bmid, bmid
	fwd[off+fmid] = lo1
	bwd[off+bmid] = hi1
	cost := 0
	for {
		// Grow the forward diagonal range by one on each side where the box
		// allows it; sentinels make out-of-range neighbours lose every
		// comparison.
		if fmin > kmin {
			fmin--
			fwd[off+fmin-1] = -1
		} else {
			fmin++
		}
		if fmax < kmax {
			fmax++
			fwd[off+fmax+1] = -1
		} else {
			fmax--
		}
		for k := fmax; k >= fmin; k -= 2 {
			var i int
			if fwd[off+k-1] >= fwd[off+k+1] {
				i = fwd[off+k-1] + 1
			} else {
				i = fwd[off+k+1]
			}
			j := i - k
			for i < hi1 && j < hi2 && a[i] == b[j] {
				i++
				j++
			}
			fwd[off+k] = i
			if odd && bmin <= k && k <= bmax && bwd[off+k] <= i {
				return i, j
			}
		}

		if bmin > kmin {
			bmin--
			bwd[off+bmin-1] = maxInt
		} else {
			bmin++
		}
		if bmax < kmax {
			bmax++
			bwd[off+bmax+1] = maxInt
		} else {
			bmax--
		}
		for k := bmax; k >= bmin; k -= 2 {
			var i int
			if bwd[off+k-1] < bwd[off+k+1] {
				i = bwd[off+k-1]
			} else {
				i = bwd[off+k+1] - 1
			}
			j := i - k
			for i > lo1 && j > lo2 && a[i-1] == b[j-1] {
				i--
				j--
			}
			bwd[off+k] = i
			if !odd && fmin <= k && k <= fmax && i <= fwd[off+k] {
				return i, j
			}
		}
		cost++
		if cost >= d.maxCost {
			return d.bestReach(lo1, hi1, lo2, hi2, fmin, fmax, bmin, bmax)
		}
	}
}

// bestReach gives up on finding the optimal split once the edit cost grows
// too large and splits at whichever search frontier point got furthest into
// the box, which bounds the running time on very different inputs at the
// price of a possibly longer edit script.
func (d *differ) bestReach(lo1, hi1, lo2, hi2, fmin, fmax, bmin, bmax int) (int, int) {
	fbest, fi := -1, 0
	for k := fmax; k >= fmin; k -= 2 {
		i := min(d.fwd[d.off+k], hi1)
		j := i - k
		if j > hi2 {
			i, j = hi2+k, hi2
		}
		if i+j > fbest {
			fbest, fi = i+j, i
		}
	}
	bbest, bi := maxInt, 0
	for k := bmax; k >= bmin; k -= 2 {
		i := max(lo1, d.bwd[d.off+k])
		j := i - k
		if j < lo2 {
			i, j = lo2+k, lo2
		}
		if i+j < bbest {
			bbest, bi = i+j, i
		}
	}
	if (hi1+hi2)-bbest < fbest-(lo1+lo2) {
		return fi, fbest - fi
	}
	return bi, bbest - bi
}

// minCost is the edit cost after which a bisection settles for a good split
// instead of the optimal one, unless the inputs are large enough to warrant
// searching longer (about the square root of their combined length).
const minCost = 256

const maxInt = int(^uint(0) >> 1)

// compact slides every change group of x (changed marks cx) up and down over
// equal lines, merging groups it runs into, and leaves it at its lowest
// position unless some position lines its end up with a change group in the
// other sequence (marks co), which git prefers.
func compact(x []int32, cx, co []bool) {
	nx, no := len(cx), len(co)
	runEnd := func(c []bool, i int) int {
		for i < len(c) && c[i] {
			i++
		}
		return i
	}
	runStart := func(c []bool, i int) int {
		for i > 0 && c[i-1] {
			i--
		}
		return i
	}
	s, os := 0, 0
	for {
		// Advance both sequences over equal lines to the next group.
		for s < nx && !cx[s] {
			s++
			for os < no && co[os] {
				os++
			}
			os++
		}
		if s >= nx {
			return
		}
		e := runEnd(cx, s)
		oe := runEnd(co, os)
		for {
			size := e - s
			for s > 0 && x[s-1] == x[e-1] {
				s--
				e--
				cx[s] = true
				cx[e] = false
				s = runStart(cx, s)
				// The other group moves back over one equal line.
				oe = os - 1
				os = runStart(co, oe)
			}
			earliestEnd := e
			endMatch := -1
			if oe > os {
				endMatch = e
			}
			for e < nx && x[s] == x[e] {
				cx[s] = false
				cx[e] = true
				s++
				e = runEnd(cx, e+1)
				os = oe + 1
				oe = runEnd(co, os)
				if oe > os {
					endMatch = e
				}
			}
			if e-s == size {
				if e > earliestEnd && endMatch >= 0 {
					for e > endMatch {
						s--
						e--
						cx[s] = true
						cx[e] = false
						oe = os - 1
						os = runStart(co, oe)
					}
				}
				break
			}
		}
		s, os = e, oe
	}
}

// hunks converts change marks into hunks.
func hunks(ca, cb []bool) []hunk {
	var out []hunk
	i, j := 0, 0
	for i < len(ca) || j < len(cb) {
		if i < len(ca) && j < len(cb) && !ca[i] && !cb[j] {
			i++
			j++
			continue
		}
		h := hunk{A: i, B: j}
		for i < len(ca) && ca[i] {
			i++
		}
		for j < len(cb) && cb[j] {
			j++
		}
		h.NA, h.NB = i-h.A, j-h.B
		out = append(out, h)
	}
	return out
}
