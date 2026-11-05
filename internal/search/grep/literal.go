package grep

import (
	"bytes"
	"context"
	"regexp/syntax"
	"strings"
	"unicode"
)

// Literal prefiltering: every match of most patterns must contain one of a
// few literal strings. Scanning for those and verifying around the hit
// skips nearly all of a file without running the regexp; the regexp then
// only sees candidate lines. When the pattern is exactly such a set of
// literals, a verified hit is the match and the regexp never runs. Builds
// with GOEXPERIMENT=simd scan for the two rarest bytes of a literal at once
// (simd.go); others scan for the rarest with bytes.IndexByte (nosimd.go).

// maxAlts caps the literal set size; larger alternations scan too often to
// beat the regexp.
const maxAlts = 8

// lit is one required literal. fold literals are ASCII-only, stored lower
// case, and compared ASCII case-insensitively; they never contain a letter
// whose simple case folding reaches outside ASCII (k: KELVIN SIGN, s: LONG
// S), so an ASCII comparison finds every (?i) match.
type lit struct {
	s    string
	fold bool
}

// litSet describes what a regexp node's matches contain: one of alts
// (none when ok is false), and, when exact, nothing but one of alts.
type litSet struct {
	alts  []lit
	exact bool
	ok    bool
}

// literals derives re's litSet.
func literals(re *syntax.Regexp) litSet {
	switch re.Op {
	case syntax.OpLiteral:
		return literalRunes(re.Rune, re.Flags&syntax.FoldCase != 0)
	case syntax.OpEmptyMatch:
		return litSet{alts: []lit{{}}, exact: true, ok: true}
	case syntax.OpCapture:
		return literals(re.Sub[0])
	case syntax.OpPlus:
		s := literals(re.Sub[0])
		s.exact = false
		return s
	case syntax.OpRepeat:
		if re.Min < 1 {
			return litSet{}
		}
		s := literals(re.Sub[0])
		s.exact = false
		return s
	case syntax.OpAlternate:
		var out litSet
		out.ok, out.exact = true, true
		for _, sub := range re.Sub {
			s := literals(sub)
			if !s.ok || len(out.alts)+len(s.alts) > maxAlts {
				return litSet{}
			}
			out.alts = append(out.alts, s.alts...)
			out.exact = out.exact && s.exact
		}
		return out
	case syntax.OpConcat:
		return concatLiterals(re.Sub)
	}
	return litSet{}
}

// literalRunes is the litSet of a literal: exact unless folding forces it
// down to its longest run of ASCII characters whose folds stay ASCII.
func literalRunes(rs []rune, fold bool) litSet {
	if !fold {
		return litSet{alts: []lit{{s: string(rs)}}, exact: true, ok: true}
	}
	var best, cur strings.Builder
	whole, letters := true, false
	flush := func() {
		if cur.Len() > best.Len() {
			best.Reset()
			best.WriteString(cur.String())
		}
		cur.Reset()
	}
	for _, r := range rs {
		if !asciiFoldSafe(r) {
			whole = false
			flush()
			continue
		}
		if unicode.IsLetter(r) {
			letters = true
		}
		cur.WriteRune(unicode.ToLower(r))
	}
	flush()
	if best.Len() == 0 {
		return litSet{}
	}
	return litSet{alts: []lit{{s: best.String(), fold: letters}}, exact: whole, ok: true}
}

// asciiFoldSafe reports an ASCII rune whose whole simple-folding orbit is
// ASCII.
func asciiFoldSafe(r rune) bool {
	if r >= 0x80 {
		return false
	}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f >= 0x80 {
			return false
		}
	}
	return true
}

// concatLiterals combines a concatenation's children: adjacent exact
// children multiply into longer literals (while the product stays small and
// their fold modes agree), and the best resulting factor represents the
// whole. It is exact only if every child merged into that one factor.
func concatLiterals(subs []*syntax.Regexp) litSet {
	var best, cur litSet
	allExact := true
	merged := 0 // children merged into cur
	consider := func(s litSet) {
		if s.ok && usable(s) && (!best.ok || cost(s) < cost(best)) {
			best = s
		}
	}
	for _, sub := range subs {
		s := literals(sub)
		if !s.exact {
			allExact = false
		}
		if s.ok && s.exact && cur.ok {
			if p, ok := product(cur, s); ok {
				cur = p
				merged++
				continue
			}
		}
		consider(cur)
		if s.ok && s.exact {
			cur, merged = s, 1
			continue
		}
		cur, merged = litSet{}, 0
		consider(s)
	}
	consider(cur)
	if best.ok {
		best.exact = allExact && merged == len(subs) && sameSet(best, cur)
	}
	return best
}

// product is a × b when both are small enough and fold-compatible.
func product(a, b litSet) (litSet, bool) {
	if len(a.alts)*len(b.alts) > maxAlts {
		return litSet{}, false
	}
	out := litSet{exact: true, ok: true}
	for _, x := range a.alts {
		for _, y := range b.alts {
			fold := x.fold || y.fold
			if fold && (hasLetter(x.s) && !x.fold || hasLetter(y.s) && !y.fold) {
				return litSet{}, false
			}
			out.alts = append(out.alts, lit{s: x.s + y.s, fold: fold})
		}
	}
	return out, true
}

func sameSet(a, b litSet) bool {
	if len(a.alts) != len(b.alts) {
		return false
	}
	for i := range a.alts {
		if a.alts[i] != b.alts[i] {
			return false
		}
	}
	return true
}

func hasLetter(s string) bool {
	for i := range len(s) {
		if c := s[i] | 0x20; 'a' <= c && c <= 'z' {
			return true
		}
	}
	return false
}

// usable reports a set worth scanning for: no empty literal.
func usable(s litSet) bool {
	for _, l := range s.alts {
		if l.s == "" {
			return false
		}
	}
	return len(s.alts) > 0
}

// cost estimates how often a set's probes hit: its worst member's probe
// frequency, plus a charge per extra member scanned.
func cost(s litSet) int {
	worst := 0
	for _, l := range s.alts {
		_, c := probe(l)
		worst = max(worst, c)
	}
	return worst + 16*(len(s.alts)-1)
}

// probe picks the byte of l to scan for: the rarest, by byteRank, preferring
// later bytes on ties so longer common prefixes are skipped. A fold letter
// costs both of its cases.
func probe(l lit) (at, cost int) {
	cost = 1 << 30
	for i := range len(l.s) {
		if c := probeCost(l, i); c <= cost {
			at, cost = i, c
		}
	}
	return at, cost - min(len(l.s), 16)
}

// secondProbe picks the byte checked together with probe byte at: the
// rarest other byte, ties going to later bytes; at itself for a one-byte
// literal.
func secondProbe(l lit, at int) int {
	at2, cost := at, 1<<30
	for i := range len(l.s) {
		if c := probeCost(l, i); i != at && c <= cost {
			at2, cost = i, c
		}
	}
	return at2
}

// probeCost is the rank of byte i of l, charging both cases of a fold
// letter.
func probeCost(l lit, i int) int {
	c := int(byteRank[l.s[i]])
	if l.fold {
		if u := upper(l.s[i]); u != l.s[i] {
			c = max(c, int(byteRank[u])) + 8
		}
	}
	return c
}

func upper(c byte) byte {
	if 'a' <= c && c <= 'z' {
		return c - 32
	}
	return c
}

// prefilter finds candidate positions of a pattern's required literals.
type prefilter struct {
	members []member
}

// member scans for one literal. Candidates are starts where probe bytes k
// (at offsets at and at2) match: byte c matches when c|mask[k] == want[k],
// mask being 0x20 for a fold letter (both cases match) and 0 otherwise.
// Byte searches look for the cases of the first probe byte (equal unless a
// fold letter). Then the whole literal is compared.
type member struct {
	lit     []byte
	fold    bool
	at, at2 int
	mask    [2]byte
	want    [2]byte
	cases   [2]byte
}

// prefState caches, per member, the next verified start (and what the
// build's probe search keeps, a probeCache), so scanning one buffer with
// increasing positions never rescans a byte. Reset it per buffer.
type prefState struct {
	start [maxAlts]int
	probe [maxAlts]probeCache
}

// unknown marks a cache slot that must be recomputed; -1 means "none up to
// the end of the buffer".
const unknown = -2

func (st *prefState) reset() {
	for i := range st.start {
		st.start[i] = unknown
		st.probe[i].reset()
	}
}

// buildPrefilter derives the prefilter of a simplified tree, and whether a
// prefilter hit alone proves a match.
func buildPrefilter(re *syntax.Regexp) (*prefilter, bool) {
	s := literals(re)
	if !s.ok || !usable(s) {
		return nil, false
	}
	p := &prefilter{}
	for _, l := range s.alts {
		at, _ := probe(l)
		at2 := secondProbe(l, at)
		p.members = append(p.members, member{
			lit:   []byte(l.s),
			fold:  l.fold,
			at:    at,
			at2:   at2,
			mask:  [2]byte{foldMask(l, at), foldMask(l, at2)},
			want:  [2]byte{l.s[at], l.s[at2]},
			cases: [2]byte{l.s[at], l.s[at] &^ foldMask(l, at)},
		})
	}
	return p, s.exact
}

// foldMask is 0x20 when byte i of l is a fold letter (matching both
// cases), else 0.
func foldMask(l lit, i int) byte {
	if l.fold && upper(l.s[i]) != l.s[i] {
		return 0x20
	}
	return 0
}

// find returns the smallest position ≥ from where some member literal
// starts, or -1. Successive calls on one buffer must not decrease from.
func (p *prefilter) find(ctx context.Context, buf []byte, from int, st *prefState) int {
	best := -1
	for i := range p.members {
		if ctx.Err() != nil {
			return -1
		}
		if s := p.members[i].next(ctx, buf, from, &st.start[i], &st.probe[i]); s >= 0 && (best < 0 || s < best) {
			best = s
		}
	}
	return best
}

// next returns the first start ≥ from of m's literal, using and updating
// the member's caches.
func (m *member) next(ctx context.Context, buf []byte, from int, start *int, pc *probeCache) int {
	if *start == -1 || *start >= from {
		return *start
	}
	n := len(m.lit)
	cancelable := ctx.Done() != nil
	for pos := from; ; {
		if ctx.Err() != nil {
			return -1
		}
		end := len(buf)
		if cancelable {
			// Include the literal's tail so starts spanning the next chunk
			// boundary are still checked exactly once.
			end = min(end, pos+scanChunk+n-1)
		}
		s := m.candidate(buf[:end], pos, pc)
		if end < len(buf) {
			pc.extend() // a chunk's miss is not a whole-buffer miss
		}
		if s < 0 {
			if end < len(buf) {
				pos = end - n + 1
				continue
			}
			*start = -1
			return -1
		}
		if m.equal(buf[s : s+n]) {
			*start = s
			return s
		}
		pos = s + 1
	}
}

// candidateBytes is candidate one start at a time, over the starts [pos,
// end).
func (m *member) candidateBytes(buf []byte, pos, end int) int {
	for s := pos; s < end; s++ {
		if buf[s+m.at]|m.mask[0] == m.want[0] && buf[s+m.at2]|m.mask[1] == m.want[1] {
			return s
		}
	}
	return -1
}

// equal compares a candidate with the literal.
func (m *member) equal(b []byte) bool {
	if !m.fold {
		return bytes.Equal(b, m.lit)
	}
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 32
		}
		if c != m.lit[i] {
			return false
		}
	}
	return true
}

// byteRank orders bytes by how common they are in source code (0 never
// seen, 255 most frequent; log scale), measured over the Rust, TypeScript,
// and Go sources of pi and zed. Probing for the rarest byte of a literal
// minimises false candidates.
var byteRank = [256]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 211, 210, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 25, 0, 0, 0, 0,
	234, 171, 202, 170, 159, 132, 177, 165, 202, 202, 181, 161, 201, 187, 202, 191,
	182, 178, 173, 168, 166, 166, 164, 161, 162, 159, 201, 196, 173, 194, 182, 173,
	157, 182, 175, 183, 176, 183, 172, 164, 167, 179, 153, 162, 174, 179, 175, 177,
	179, 146, 182, 187, 183, 170, 163, 166, 150, 151, 142, 177, 175, 177, 131, 199,
	179, 215, 194, 208, 208, 223, 201, 200, 201, 214, 171, 190, 210, 203, 216, 215,
	206, 172, 215, 215, 220, 205, 191, 194, 193, 194, 168, 191, 178, 191, 128, 50,
	159, 118, 134, 110, 114, 106, 127, 137, 132, 112, 105, 112, 117, 114, 106, 113,
	141, 117, 126, 112, 160, 141, 132, 122, 115, 110, 108, 104, 118, 105, 101, 129,
	111, 104, 105, 106, 118, 120, 126, 118, 115, 122, 104, 119, 108, 108, 105, 107,
	112, 104, 105, 102, 106, 107, 103, 139, 121, 112, 111, 119, 111, 109, 105, 106,
	0, 0, 143, 128, 94, 92, 69, 83, 61, 101, 75, 135, 110, 32, 112, 98,
	119, 112, 70, 73, 55, 71, 53, 109, 111, 105, 83, 90, 25, 25, 25, 0,
	123, 115, 165, 111, 110, 112, 119, 102, 99, 95, 102, 99, 102, 96, 90, 119,
	127, 32, 0, 79, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
}
