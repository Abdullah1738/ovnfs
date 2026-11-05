package grep

import (
	"context"
	"encoding/binary"
	"math/bits"
	"regexp/syntax"
	"slices"
	"unicode"
	"unicode/utf8"
)

// Line mode only asks whether a line contains a match, never where, so a
// lazily built DFA over the compiled program answers it in one pass over
// the bytes, several times faster than Go's backtracker or NFA. States are
// sets of program counters reached so far (plus the unanchored start), and
// transitions are computed on first use and cached per worker.
//
// Empty-width assertions (^ $ \b \B) depend on the characters on both sides
// of a position, so a state keeps its program counters before following
// assertions and remembers the previous character's kind; the closure is
// taken when the next character (or the line end) is known. Word
// boundaries are ASCII, as in Go's regexp, so a line matches here exactly
// when regexp.Match accepts it. A state only remembers what the program can
// observe: without \b or \B it never records the previous character's
// kind, and without ^ it never records the line start, so states that
// differ only in unobservable context are one state.
//
// Self-loop acceleration: most scans spend their time in a state that most
// bytes lead straight back to (the start state of `\p{Greek}` on ASCII
// text, of `\d{4}` on non-digits). The first time such a self-loop is
// computed, the state's exact self-loop byte set is derived from the
// program; when it covers most of ASCII the state is accelerated: its
// self-loop transitions are marked, and the scan jumps to the next byte
// that can leave the state with findExit, a vector search in builds with
// GOEXPERIMENT=simd (simd.go) and a byte loop otherwise.

// maxDFAStates bounds the cache; past it the cache restarts, which keeps
// memory flat for patterns whose state count explodes.
const maxDFAStates = 4096

// scanChunk bounds uninterrupted DFA transitions, accelerated skips, and
// literal candidate searches for cancellable scans. Background scans keep
// the original whole-buffer fast paths.
const scanChunk = 64 << 10

// loopMark is the transition table entry of an accelerated state's
// self-loop byte: the state stays the same, and next skips ahead to the
// state's next exit byte.
const loopMark int32 = -2

// minLoopBytes is how many of the 128 ASCII bytes (counting '\n' when it
// keeps the state) must loop back to a state for it to be accelerated.
const minLoopBytes = 96

// maxAccelRanges bounds the byte ranges of an accelerated state's exit set
// (each costs findExit a vector subtract, saturating subtract and min).
const maxAccelRanges = 6

// byteRange is the bytes [lo, lo+span].
type byteRange struct{ lo, span uint8 }

// accel is a state's self-loop acceleration status.
type accel uint8

const (
	accelUnknown accel = iota
	accelNo
	accelYes
)

// high is which runes >= 0x80 (every rune a byte >= 0x80 starts decodes
// to one, utf8.RuneError included) a rune instruction matches.
type high uint8

const (
	highUnknown high = iota
	highNone
	highAll
	highSome
)

// asciiSet is a set of ASCII bytes: bit c&63 of word c>>6.
type asciiSet [2]uint64

func (s asciiSet) has(c byte) bool         { return c < utf8.RuneSelf && s[c>>6]>>(c&63)&1 != 0 }
func (s *asciiSet) add(c byte)             { s[c>>6] |= 1 << (c & 63) }
func (s asciiSet) and(t asciiSet) asciiSet { return asciiSet{s[0] & t[0], s[1] & t[1]} }
func (s asciiSet) or(t asciiSet) asciiSet  { return asciiSet{s[0] | t[0], s[1] | t[1]} }
func (s asciiSet) andNot(t asciiSet) asciiSet {
	return asciiSet{s[0] &^ t[0], s[1] &^ t[1]}
}
func (s asciiSet) len() int { return bits.OnesCount64(s[0]) + bits.OnesCount64(s[1]) }

// asciiBytes is every ASCII byte but '\n' (line ends never take a
// transition: next handles them itself); wordBytes and nonwordBytes split
// it by \b's notion of a word character.
var asciiBytes, wordBytes, nonwordBytes = asciiClasses()

func asciiClasses() (all, word, nonword asciiSet) {
	for c := range byte(utf8.RuneSelf) {
		if c == '\n' {
			continue
		}
		all.add(c)
		if syntax.IsWordChar(rune(c)) {
			word.add(c)
		}
	}
	return all, word, all.andNot(word)
}

// dfa is one worker's cache over a shared, read-only program.
type dfa struct {
	prog *syntax.Prog
	// trans holds 128 ASCII transitions per state: 0 unknown, -1 match,
	// loopMark a self-loop of an accelerated state, k+1 state k. The '\n'
	// column is never filled: next handles line ends itself.
	trans  []int32
	states []dstate
	index  map[string]int32
	// lineCtx: the program tests ^ or \A, so states record the line start.
	// wordCtx: it tests \b or \B, so states record the previous
	// character's kind.
	lineCtx, wordCtx bool
	// Per instruction, computed on first use by acceleration: the ASCII
	// bytes (but '\n') a rune instruction matches, and its runes >= 0x80.
	instASCII []asciiSet
	instHigh  []high

	// scratch for closures and state keys
	stack []uint32
	mark  []uint32 // mark[pc] == gen: pc seen in this closure
	gen   uint32
	runes []uint32
	set   []uint32
	key   []byte
}

// dstate is one DFA state.
type dstate struct {
	pcs   []uint32 // instructions to resume from, before assertions
	flags uint8
	eol   int8 // match at the line end: 0 unknown, 1 yes, -1 no
	// accel is decided the first time a self-loop of the state is computed.
	accel accel
	// nlLoop: accelerated state 0 without a match at the line end, so '\n'
	// leads back to it too and the skip crosses lines.
	nlLoop bool
	// Accelerated: the bytes that may leave the state, as a table for
	// byte loops and as ranges for vectors.
	exits  *[256]bool
	ranges []byteRange
	wide   map[rune]int32
}

const (
	atLineStart uint8 = 1 << iota
	afterWord
)

func newDFA(prog *syntax.Prog) *dfa {
	n := len(prog.Inst)
	d := &dfa{
		prog:      prog,
		mark:      make([]uint32, n),
		instASCII: make([]asciiSet, n),
		instHigh:  make([]high, n),
	}
	for _, inst := range prog.Inst {
		if inst.Op != syntax.InstEmptyWidth {
			continue
		}
		op := syntax.EmptyOp(inst.Arg)
		d.lineCtx = d.lineCtx || op&(syntax.EmptyBeginLine|syntax.EmptyBeginText) != 0
		d.wordCtx = d.wordCtx || op&(syntax.EmptyWordBoundary|syntax.EmptyNoWordBoundary) != 0
	}
	d.reset()
	return d
}

// reset empties the cache, leaving the start state as state 0.
func (d *dfa) reset() {
	d.trans = d.trans[:0]
	d.states = d.states[:0]
	d.index = make(map[string]int32)
	d.set = append(d.set[:0], uint32(d.prog.Start))
	var flags uint8
	if d.lineCtx {
		flags = atLineStart
	}
	d.add(flags)
}

// add interns d.set with flags and returns its state index (after a reset
// when the cache is full).
func (d *dfa) add(flags uint8) int32 {
	slices.Sort(d.set)
	d.set = slices.Compact(d.set)
	d.key = append(d.key[:0], flags)
	for _, pc := range d.set {
		d.key = binary.LittleEndian.AppendUint32(d.key, pc)
	}
	if i, ok := d.index[string(d.key)]; ok {
		return i
	}
	if len(d.states) >= maxDFAStates {
		set := append([]uint32(nil), d.set...)
		d.reset()
		d.set = set
		return d.add(flags)
	}
	i := int32(len(d.states))
	d.states = append(d.states, dstate{pcs: slices.Clone(d.set), flags: flags})
	d.trans = append(d.trans, make([]int32, 128)...)
	d.index[string(d.key)] = i
	return i
}

// flagsAfter returns the flags of every state reached by a character of
// the given kind (only what the program can observe).
func (d *dfa) flagsAfter(word bool) uint8 {
	if word && d.wordCtx {
		return afterWord
	}
	return 0
}

// isWord is Go's \b notion of a word character.
func isWord(r rune) bool {
	return r < utf8.RuneSelf && syntax.IsWordChar(r)
}

// emptyContext is the empty-width context of a position after a state with
// flags, before the line end (end) or a character of the given kind.
func emptyContext(flags uint8, end, nextWord bool) syntax.EmptyOp {
	var ctx syntax.EmptyOp
	if flags&atLineStart != 0 {
		ctx |= syntax.EmptyBeginLine | syntax.EmptyBeginText
	}
	if end {
		ctx |= syntax.EmptyEndLine | syntax.EmptyEndText
	}
	if (flags&afterWord != 0) != nextWord {
		ctx |= syntax.EmptyWordBoundary
	} else {
		ctx |= syntax.EmptyNoWordBoundary
	}
	return ctx
}

// closure follows the empty-width closure of pcs under ctx, collecting the
// rune instructions it reaches in d.runes. It reports whether it reaches a
// match (d.runes is then incomplete).
func (d *dfa) closure(pcs []uint32, ctx syntax.EmptyOp) bool {
	d.gen++
	if d.gen == 0 {
		clear(d.mark)
		d.gen = 1
	}
	d.stack = append(d.stack[:0], pcs...)
	d.runes = d.runes[:0]
	for len(d.stack) > 0 {
		pc := d.stack[len(d.stack)-1]
		d.stack = d.stack[:len(d.stack)-1]
		if d.mark[pc] == d.gen {
			continue
		}
		d.mark[pc] = d.gen
		inst := &d.prog.Inst[pc]
		switch inst.Op {
		case syntax.InstMatch:
			return true
		case syntax.InstAlt, syntax.InstAltMatch:
			d.stack = append(d.stack, inst.Arg, inst.Out)
		case syntax.InstCapture, syntax.InstNop:
			d.stack = append(d.stack, inst.Out)
		case syntax.InstEmptyWidth:
			if syntax.EmptyOp(inst.Arg)&^ctx == 0 {
				d.stack = append(d.stack, inst.Out)
			}
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			d.runes = append(d.runes, pc)
		}
	}
	return false
}

// consumes reports whether rune instruction inst matches r.
func consumes(inst *syntax.Inst, r rune) bool {
	switch inst.Op {
	case syntax.InstRuneAny:
		return true
	case syntax.InstRuneAnyNotNL:
		return r != '\n'
	}
	return inst.MatchRune(r)
}

// step computes the transition of state s on r (end: the line end instead
// of a character): -1 when a match completes, else a state index + 1; at
// the line end, 0 means no match.
func (d *dfa) step(s int32, r rune, end bool) int32 {
	st := &d.states[s]
	nextWord := !end && isWord(r)
	if d.closure(st.pcs, emptyContext(st.flags, end, nextWord)) {
		return -1
	}
	if end {
		return 0
	}
	d.set = append(d.set[:0], uint32(d.prog.Start))
	for _, pc := range d.runes {
		if inst := &d.prog.Inst[pc]; consumes(inst, r) {
			d.set = append(d.set, inst.Out)
		}
	}
	return d.add(d.flagsAfter(nextWord)) + 1
}

// eol reports whether state s matches at the line end.
func (d *dfa) eol(s int32) bool {
	st := &d.states[s]
	if st.eol == 0 {
		st.eol = -1
		if d.step(s, 0, true) == -1 {
			st.eol = 1
		}
	}
	return st.eol == 1
}

// instClass fills d.instASCII[pc] and d.instHigh[pc] on first use: what
// rune instruction pc matches, exactly as step tests it.
func (d *dfa) instClass(pc uint32) {
	if d.instHigh[pc] != highUnknown {
		return
	}
	inst := &d.prog.Inst[pc]
	var m asciiSet
	for c := range byte(utf8.RuneSelf) {
		if consumes(inst, rune(c)) {
			m.add(c)
		}
	}
	d.instASCII[pc] = m.and(asciiBytes)
	d.instHigh[pc] = highClass(inst)
}

// highClass returns which runes >= 0x80 rune instruction inst matches, by
// MatchRune's rules: case folding applies to single runes only, and pairs
// are sorted.
func highClass(inst *syntax.Inst) high {
	switch inst.Op {
	case syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
		return highAll
	}
	rs := inst.Rune
	if len(rs) == 1 {
		r0 := rs[0]
		if r0 >= utf8.RuneSelf {
			return highSome
		}
		if syntax.Flags(inst.Arg)&syntax.FoldCase != 0 {
			for r1 := unicode.SimpleFold(r0); r1 != r0; r1 = unicode.SimpleFold(r1) {
				if r1 >= utf8.RuneSelf {
					return highSome
				}
			}
		}
		return highNone
	}
	// Walk the sorted pairs, extending the covered prefix of [0x80, max].
	covered, any := rune(utf8.RuneSelf), false
	for i := 0; i+1 < len(rs); i += 2 {
		if rs[i+1] < utf8.RuneSelf {
			continue
		}
		any = true
		if rs[i] <= covered && rs[i+1] >= covered {
			covered = rs[i+1] + 1
		}
	}
	switch {
	case !any:
		return highNone
	case covered > unicode.MaxRune:
		return highAll
	}
	return highSome
}

// loopPass is one character kind analyze checks: its ASCII bytes, and
// whether runes >= 0x80 (all non-word) are of that kind.
type loopPass struct {
	word  bool
	bytes asciiSet
	high  bool
}

var (
	// The kind is unobservable: one closure serves every byte.
	anyKindPasses = []loopPass{{bytes: asciiBytes, high: true}}
	wordPasses    = []loopPass{{bytes: nonwordBytes, high: true}, {word: true, bytes: wordBytes}}
)

// analyze decides whether state s is accelerated. It sets its accel status
// and, when accelerated, its exit ranges, and marks its self-loop bytes in
// its transition row with loopMark.
//
// A byte c of a given kind (word / non-word) leads from s back to s
// exactly when its closure reaches no match, the flags after c equal s's,
// and the set step builds, {start} plus the out of every rune instruction
// in the closure matching c, equals s's pcs: every matching instruction's
// out is in pcs, and every pc of pcs but start is the out of some matching
// instruction. That is computed for all bytes at once with 128-bit sets.
// Runes >= 0x80 (all non-word) count as one more byte when every
// instruction matches all of them or none of them; otherwise the bytes >=
// 0x80 are exits.
func (d *dfa) analyze(s int32) {
	d.states[s].accel = accelNo
	pcs, flags := d.states[s].pcs, d.states[s].flags
	// Every character clears the line start.
	if flags&atLineStart != 0 {
		return
	}
	start := uint32(d.prog.Start)
	if !slices.Contains(pcs, start) {
		return
	}
	insts := d.prog.Inst

	passes := anyKindPasses
	if d.wordCtx {
		passes = wordPasses
	}
	var loopASCII asciiSet
	loopHigh := false
	for _, p := range passes {
		if d.flagsAfter(p.word) != flags || d.closure(pcs, emptyContext(flags, false, p.word)) {
			continue
		}
		ok, okHigh := p.bytes, p.high
		for _, pc := range d.runes {
			d.instClass(pc)
			h := d.instHigh[pc]
			if h == highSome {
				okHigh = false
			}
			if !slices.Contains(pcs, insts[pc].Out) {
				ok = ok.andNot(d.instASCII[pc])
				if h != highNone {
					okHigh = false
				}
			}
		}
		for _, q := range pcs {
			if q == start {
				continue
			}
			var cover asciiSet
			coverHigh := false
			for _, pc := range d.runes {
				if insts[pc].Out == q {
					cover = cover.or(d.instASCII[pc])
					coverHigh = coverHigh || d.instHigh[pc] == highAll
				}
			}
			ok = ok.and(cover)
			okHigh = okHigh && coverHigh
		}
		loopASCII = loopASCII.or(ok)
		loopHigh = loopHigh || okHigh
	}

	// After '\n' the scan restarts in state 0, so for state 0 a line end
	// without a match is one more self-loop.
	nlLoop := s == 0 && !d.eol(0)
	n := loopASCII.len()
	if nlLoop {
		n++
	}
	if n < minLoopBytes {
		return
	}

	exits := new([256]bool)
	for b := range exits {
		switch {
		case b >= utf8.RuneSelf:
			exits[b] = !loopHigh
		case b == '\n':
			exits[b] = !nlLoop
		default:
			exits[b] = !loopASCII.has(byte(b))
		}
	}
	var ranges []byteRange
	for b := 0; b < 256; {
		if !exits[b] {
			b++
			continue
		}
		lo := b
		for b < 256 && exits[b] {
			b++
		}
		if len(ranges) == maxAccelRanges {
			return
		}
		ranges = append(ranges, byteRange{lo: uint8(lo), span: uint8(b - 1 - lo)})
	}

	st := &d.states[s]
	st.accel, st.nlLoop, st.exits, st.ranges = accelYes, nlLoop, exits, ranges
	row := d.trans[int(s)<<7:][:128]
	for c := range row {
		if loopASCII.has(byte(c)) {
			row[c] = loopMark
		}
	}
}

// transition returns the cached or computed transition of s on r.
func (d *dfa) transition(s int32, r rune) int32 {
	if r < utf8.RuneSelf {
		idx := int(s)<<7 | int(r)
		switch t := d.trans[idx]; t {
		case loopMark:
			return s + 1
		case 0:
		default:
			return t
		}
		n := len(d.states)
		t := d.step(s, r, false)
		if len(d.states) < n {
			return t // the cache restarted; s is gone
		}
		d.trans[idx] = t
		if t == s+1 && d.states[s].accel == accelUnknown {
			d.analyze(s)
		}
		return t
	}
	if t, ok := d.states[s].wide[r]; ok {
		return t
	}
	n := len(d.states)
	t := d.step(s, r, false)
	if len(d.states) >= n {
		st := &d.states[s]
		if st.wide == nil {
			st.wide = make(map[rune]int32)
		}
		st.wide[r] = t
	}
	return t
}

// skipHead is how many bytes skip checks one at a time before calling
// findExit: most skips inside a line end sooner, and vector setup (and the
// call) would cost more than it saves.
const skipHead = 64

// skip returns the index of the first byte at or after from that may leave
// accelerated state s (or len(buf)). Every byte skipped leads back to s,
// and none is '\n' unless s.nlLoop.
func (d *dfa) skip(s int32, buf []byte, from int) int {
	st := &d.states[s]
	if len(st.ranges) == 0 {
		return len(buf)
	}
	head := min(len(buf), from+skipHead)
	if i := findExitBytes(st.exits, buf[:head], from); i < head || head == len(buf) {
		return i
	}
	return findExit(st, buf, head)
}

// lineStart returns the start of the line holding position i, given ls, a
// line start at or before it: ls itself unless crossed, which says that
// skips may have passed newlines since ls. Resolving it only for a
// reported line spares the search on lines that do not match (all of
// them, for dfa.matches, whose buffer holds no newline).
func lineStart(buf []byte, ls, i int, crossed bool) int {
	if crossed {
		return ls + lastNewline(buf[ls:i]) + 1
	}
	return ls
}

// findExitBytes returns the first index at or after from whose byte is in
// exits, or len(buf), one byte at a time.
func findExitBytes(exits *[256]bool, buf []byte, from int) int {
	for i := from; i < len(buf); i++ {
		if exits[buf[i]] {
			return i
		}
	}
	return len(buf)
}

// next returns the first line [ls, le) at or after from (a line start) that
// contains a match; le is the index of its '\n' or len(buf).
func (d *dfa) next(ctx context.Context, buf []byte, from int) (ls, le int, ok bool) {
	s := int32(0)
	ls = from
	crossed := false // see lineStart
	poll := len(buf)
	cancelable := ctx.Done() != nil
	if cancelable {
		poll = from
	}
	for i := from; i < len(buf); {
		if i >= poll {
			if ctx.Err() != nil {
				return 0, 0, false
			}
			poll = i + scanChunk
		}
		c := buf[i]
		var t int32
		if c < utf8.RuneSelf {
			t = d.trans[int(s)<<7|int(c)]
			if t > 0 {
				s, i = t-1, i+1
				continue
			}
			if t == loopMark {
				// ASCII bytes are rune boundaries, so the skip resumes
				// decoding exactly where the per-byte loop would.
				end := len(buf)
				if cancelable {
					end = min(end, poll)
				}
				i = d.skip(s, buf[:end], i+1)
				crossed = crossed || d.states[s].nlLoop
				continue
			}
			if c == '\n' {
				if d.eol(s) {
					return lineStart(buf, ls, i, crossed), i, true
				}
				s, i = 0, i+1
				ls, crossed = i, false
				continue
			}
			if t == 0 {
				t = d.transition(s, rune(c))
			}
			i++
		} else {
			r, n := utf8.DecodeRune(buf[i:])
			t = d.transition(s, r)
			i += n
		}
		if t < 0 {
			return lineStart(buf, ls, i, crossed), lineEnd(buf, i), true
		}
		s = t - 1
	}
	if ls = lineStart(buf, ls, len(buf), crossed); ls < len(buf) && d.eol(s) {
		return ls, len(buf), true
	}
	return 0, 0, false
}

// matches reports whether line (without its '\n') contains a match.
func (d *dfa) matches(ctx context.Context, line []byte) bool {
	// next reads an empty buffer as no line at all; here it is one empty
	// line, which matches when the start state matches at its end.
	if len(line) == 0 {
		return d.eol(0)
	}
	_, _, ok := d.next(ctx, line, 0)
	return ok
}
