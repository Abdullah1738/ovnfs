package grep

import (
	"bytes"
	"context"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// binaryWindow is how much of a buffer is checked for NUL up front, as
// grep-searcher does for slices (its 64 KiB buffer size). A NUL there makes
// the file binary: searched, but without matches. Past the window, a line
// about to be reported (match or context) that holds a NUL ends the file's
// search, keeping what was reported before it.
const binaryWindow = 64 << 10

// scanParams says what one buffer's scan reports.
type scanParams struct {
	content       bool // build Match entries (line numbers, text, context)
	before, after int  // context lines (content only)
	maxColumns    int  // truncate reported lines longer than this many bytes
	cap           int  // stop after this many matches; 0: no cap
	probe         bool // after cap, look for one more match (sets more, cuts after-context)
}

// scanner searches buffers for one compiled pattern. It is per goroutine:
// its read buffer, prefilter state, and result storage are reused, and the
// strings and context slices it hands out are carved from append-only slabs,
// so a steady-state search allocates only when slabs fill up.
type scanner struct {
	c      *compiled
	st     prefState
	dfa    *dfa          // line mode
	buf    []byte        // file read buffer
	text   []byte        // slab for line and path strings
	ctx    []ContextLine // slab for context slices
	starts []int         // before-context scratch
	out    []Match       // matches of the files scanned so far
}

func newScanner(c *compiled) *scanner {
	sc := &scanner{c: c}
	if c.prog != nil {
		sc.dfa = newDFA(c.prog)
	}
	return sc
}

// scan appends buf's matches to sc.out (when p.content) and reports how
// many matches it kept (collected, at most p.cap), how many it found
// (count: collected plus the probe match), and whether a match exists past
// the cap (more).
func (sc *scanner) scan(ctx context.Context, buf []byte, p scanParams) (collected, count int, more bool) {
	if ctx.Err() != nil {
		return 0, 0, false
	}
	c := sc.c
	if c.never || bytes.IndexByte(buf[:min(len(buf), binaryWindow)], 0) >= 0 {
		return 0, 0, false
	}
	sc.st.reset()
	it := blocks{ctx: ctx, c: c, buf: buf, st: &sc.st, dfa: sc.dfa}
	if c.multiline && c.pre != nil && c.pre.find(ctx, buf, 0, &sc.st) < 0 {
		return 0, 0, false
	}
	nul := func(s, e int) bool { return e > binaryWindow && bytes.IndexByte(buf[s:e], 0) >= 0 }

	var (
		cur, curLine = 0, 1 // a line start and its 1-based number
		floor        int    // lines before floor were reported already
		last         = -1   // sc.out index of the latest match
		afterPos     int    // next after-context line of the latest match
		afterLine    int
		afterLeft    int
	)
	// flushAfter reports pending after-context lines that start before
	// limit; false means a NUL ended the search.
	flushAfter := func(limit int) bool {
		for afterLeft > 0 && afterPos < limit && afterPos < len(buf) {
			if ctx.Err() != nil {
				return false
			}
			e := lineEnd(buf, afterPos)
			if nul(afterPos, e) {
				return false
			}
			if last >= 0 {
				m := &sc.out[last]
				text, _ := sc.line(ctx, buf[afterPos:e], p.maxColumns)
				m.After = append(m.After, ContextLine{Line: afterLine, Text: text})
			}
			afterPos, afterLine, afterLeft = e+1, afterLine+1, afterLeft-1
			floor = afterPos
		}
		return true
	}
	// beforeStarts collects up to p.before unreported line starts preceding
	// bs, nearest first.
	beforeStarts := func(bs int) []int {
		sc.starts = sc.starts[:0]
		for pos := bs; len(sc.starts) < p.before && pos > floor && ctx.Err() == nil; {
			pos = lastNewline(buf[:pos-1]) + 1
			sc.starts = append(sc.starts, pos)
		}
		return sc.starts
	}

	for {
		if ctx.Err() != nil {
			return collected, count, false
		}
		capped := p.cap > 0 && collected >= p.cap
		if capped && !p.probe {
			return collected, count, false
		}
		bs, be, ok := it.next()
		if ctx.Err() != nil {
			return collected, count, false
		}
		if !ok {
			break
		}
		if capped {
			// grep-searcher reports the next match's context before the match
			// itself, so a NUL in either keeps the probe from counting.
			if p.content {
				if !flushAfter(bs) {
					return collected, count, false
				}
				for _, s := range beforeStarts(bs) {
					if nul(s, lineEnd(buf, s)) {
						return collected, count, false
					}
				}
			}
			if nul(bs, be) {
				return collected, count, false
			}
			return collected, count + 1, true
		}
		if !p.content {
			if nul(bs, be) {
				return collected, count, false
			}
			collected++
			count++
			continue
		}

		if !flushAfter(bs) {
			return collected, count, false
		}
		curLine += bytes.Count(buf[cur:bs], newline)
		cur = bs
		starts := beforeStarts(bs)
		before := sc.ctxSlice(len(starts))
		for i := range before {
			if ctx.Err() != nil {
				return collected, count, false
			}
			s := starts[len(starts)-1-i]
			e := lineEnd(buf, s)
			if nul(s, e) {
				return collected, count, false
			}
			text, _ := sc.line(ctx, buf[s:e], p.maxColumns)
			before[i] = ContextLine{Line: curLine - len(starts) + i, Text: text}
		}
		if nul(bs, be) {
			return collected, count, false
		}
		text, truncated, column := sc.matched(ctx, buf, bs, be, p.maxColumns)
		sc.out = append(sc.out, Match{
			Line:      curLine,
			Text:      text,
			Before:    before,
			After:     sc.ctxSlice(p.after)[:0],
			Truncated: truncated,
			Column:    column,
		})
		last = len(sc.out) - 1
		collected++
		count++
		afterPos, afterLeft, floor = be+1, p.after, be+1
		afterLine = curLine + 1
		if c.multiline {
			afterLine += bytes.Count(buf[bs:be], newline)
		}
	}
	if p.content {
		flushAfter(len(buf))
	}
	return collected, count, false
}

var newline = []byte{'\n'}

// lineEnd is the index of the '\n' ending the line that starts at s, or
// len(buf).
func lineEnd(buf []byte, s int) int {
	if i := bytes.IndexByte(buf[s:], '\n'); i >= 0 {
		return s + i
	}
	return len(buf)
}

// ctxSlice carves n context slots (capacity exactly n) from the slab.
func (sc *scanner) ctxSlice(n int) []ContextLine {
	if n == 0 {
		return nil
	}
	if cap(sc.ctx)-len(sc.ctx) < n {
		sc.ctx = make([]ContextLine, 0, max(1024, n))
	}
	l := len(sc.ctx)
	sc.ctx = sc.ctx[:l+n]
	return sc.ctx[l : l+n : l+n]
}

// reserve makes room for n more bytes in the text slab. Earlier strings
// keep pointing into the slabs they were carved from, which are never
// written again.
func (sc *scanner) reserve(n int) {
	if cap(sc.text)-len(sc.text) < n {
		sc.text = make([]byte, 0, max(64<<10, n))
	}
}

// str returns the slab bytes from start as a string.
func (sc *scanner) str(start int) string {
	if start == len(sc.text) {
		return ""
	}
	return unsafe.String(&sc.text[start], len(sc.text)-start)
}

// clone copies s into the text slab.
func (sc *scanner) clone(s string) string {
	sc.reserve(len(s))
	start := len(sc.text)
	sc.text = append(sc.text, s...)
	return sc.str(start)
}

// line renders a reported context line: trailing Unicode whitespace
// trimmed, invalid UTF-8 replaced by U+FFFD per maximal subpart (Rust's
// from_utf8_lossy), and with maxColumns > 0 a line longer than that many
// bytes cut to maxColumns-3 bytes at a character boundary plus "...".
// Matching lines are windowed around their match instead ([scanner.matched]).
func (sc *scanner) line(ctx context.Context, b []byte, maxColumns int) (string, bool) {
	b = trimSpaceRight(ctx, b)
	if ctx.Err() != nil {
		return "", false
	}
	sc.reserve(3*len(b) + 3)
	start := len(sc.text)
	if utf8.Valid(b) {
		sc.text = append(sc.text, b...)
	} else {
		sc.text = appendLossy(ctx, sc.text, b)
	}
	if maxColumns <= 0 || len(sc.text)-start <= maxColumns {
		return sc.str(start), false
	}
	cut := max(0, maxColumns-3)
	for cut > 0 && !utf8.RuneStart(sc.text[start+cut]) {
		cut--
	}
	sc.text = append(sc.text[:start+cut], "..."...)
	return sc.str(start), true
}

// trimSpaceRight drops trailing White_Space characters (Rust's trim_end).
func trimSpaceRight(ctx context.Context, b []byte) []byte {
	poll := -1
	if ctx.Done() != nil {
		poll = len(b)
	}
	for len(b) > 0 {
		if len(b) <= poll {
			if ctx.Err() != nil {
				return nil
			}
			poll = len(b) - scanChunk
		}
		c := b[len(b)-1]
		if c < utf8.RuneSelf {
			if c != ' ' && (c < '\t' || c > '\r') {
				return b
			}
			b = b[:len(b)-1]
			continue
		}
		r, n := utf8.DecodeLastRune(b)
		if !unicode.IsSpace(r) {
			return b
		}
		b = b[:len(b)-n]
	}
	return b
}

// appendLossy appends b with each maximal invalid UTF-8 subpart replaced
// by U+FFFD.
func appendLossy(ctx context.Context, dst, b []byte) []byte {
	poll := -1
	if ctx.Done() != nil {
		poll = len(b)
	}
	for len(b) > 0 {
		if len(b) <= poll {
			if ctx.Err() != nil {
				return dst
			}
			poll = len(b) - scanChunk
		}
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			n = invalidPrefix(b)
			dst = append(dst, "\uFFFD"...)
		} else {
			dst = append(dst, b[:n]...)
		}
		b = b[n:]
	}
	return dst
}

// invalidPrefix is the length of the maximal subpart at the start of b, an
// invalid sequence: a lead byte and the continuation bytes that could still
// have completed it (Unicode Table 3-7).
func invalidPrefix(b []byte) int {
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch c := b[0]; {
	case 0xC2 <= c && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case 0xE1 <= c && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		need, hi = 2, 0x9F
	case c == 0xF0:
		need, lo = 3, 0x90
	case 0xF1 <= c && c <= 0xF3:
		need = 3
	case c == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for n <= need && n < len(b) && lo <= b[n] && b[n] <= hi {
		n++
		lo, hi = 0x80, 0xBF
	}
	return n
}

// blocks iterates over a buffer's matches as line blocks [bs, be): be is
// the index of the '\n' ending the block's last line, or len(buf). Outside
// multiline mode every block is one line; in multiline mode a block is the
// lines a match touches, and matches on the same or adjacent lines merge
// into one block (grep-searcher's MultiLine sink).
type blocks struct {
	ctx  context.Context
	c    *compiled
	buf  []byte
	st   *prefState
	dfa  *dfa
	from int // line mode: next line start; multiline: next search position
	pend [2]int
	has  bool // pend holds a block awaiting merges
	done bool
}

func (it *blocks) next() (bs, be int, ok bool) {
	if it.ctx.Err() != nil {
		return 0, 0, false
	}
	if it.c.multiline {
		return it.nextMulti()
	}
	c, buf := it.c, it.buf
	for it.from < len(buf) && it.ctx.Err() == nil {
		from := it.from
		if c.pre != nil {
			cand := c.pre.find(it.ctx, buf, from, it.st)
			if cand < 0 {
				it.from = len(buf)
				return 0, 0, false
			}
			ls := from + lastNewline(buf[from:cand]) + 1
			le := lineEnd(buf, cand)
			it.from = le + 1
			if c.exact || it.dfa.matches(it.ctx, buf[ls:le]) {
				return ls, le, true
			}
			continue
		}
		ls, le, ok := it.dfa.next(it.ctx, buf, from)
		if !ok {
			it.from = len(buf)
			return 0, 0, false
		}
		it.from = le + 1
		return ls, le, true
	}
	return 0, 0, false
}

func (it *blocks) nextMulti() (bs, be int, ok bool) {
	buf := it.buf
	for !it.done && it.from < len(buf) && it.ctx.Err() == nil {
		loc := it.c.re.FindIndex(buf[it.from:])
		if it.ctx.Err() != nil {
			return 0, 0, false
		}
		if loc == nil {
			it.done = true
			break
		}
		s, e := it.from+loc[0], it.from+loc[1]
		it.from = e
		if s == e {
			it.from++
		}
		ls := lastNewline(buf[:s]) + 1
		le := len(buf)
		if e > ls && buf[e-1] == '\n' {
			le = e
		} else if i := bytes.IndexByte(buf[e:], '\n'); i >= 0 {
			le = e + i + 1
		}
		switch {
		case it.has && it.pend[1] >= ls:
			it.pend[1] = le
		case it.has:
			out := it.pend
			it.pend = [2]int{ls, le}
			return it.emit(out)
		default:
			it.pend, it.has = [2]int{ls, le}, true
		}
	}
	if it.has && it.ctx.Err() == nil {
		it.has = false
		return it.emit(it.pend)
	}
	return 0, 0, false
}

// emit turns a pending line range into a block; an empty range (a match
// just past a final '\n') ends the search, as in grep-searcher.
func (it *blocks) emit(r [2]int) (bs, be int, ok bool) {
	if r[0] == r[1] {
		it.done, it.has = true, false
		return 0, 0, false
	}
	if it.buf[r[1]-1] == '\n' {
		r[1]--
	}
	return r[0], r[1], true
}
