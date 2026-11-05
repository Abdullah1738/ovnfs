// Package pattern compiles glob patterns with the semantics of Rust's
// `globset` crate built with literal separators, the dialect gitignore
// rules (via the `ignore` crate) and the glob/grep filters use:
//
//	?       one byte other than '/'
//	*       any run of bytes without '/'
//	**/     leading: any (possibly empty) run of whole directories
//	/**     trailing: '/' followed by anything
//	/**/    '/' or '/…/'
//	[a-z]   byte class; [!…] and [^…] negate (negated classes match '/')
//	{a,b}   alternatives (empty alternatives are dropped)
//	\c      literal c
//
// A `**` in any other position is two `*`s, and a pattern that is exactly
// `**` matches everything. Paths are '/'-separated and relative. The only
// knowing deviation from globset: its `.` (inside the `**` forms) does not
// match '\n'; here it does.
//
// Besides [Glob.Match], a compiled glob answers the questions a walker needs
// to prune: [Glob.MayMatchUnder] (can anything below this directory match?)
// and [Glob.MaxDepth] (how deep can a match be?).
package pattern

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// ErrInvalid reports a malformed glob: an unclosed class (unless allowed),
// a dangling escape, unbalanced braces, an inverted range, or more than
// maxAlternatives alternatives.
var ErrInvalid = errors.New("invalid glob")

// Options select dialect details.
type Options struct {
	// AllowUnclosedClass treats a '[' without a closing ']' as a literal
	// (gitignore rules) instead of rejecting the glob.
	AllowUnclosedClass bool
}

// maxAlternatives bounds `{a,b}` expansion.
const maxAlternatives = 256

// Glob is a compiled pattern; any alternative matching selects it. It is
// immutable and safe for concurrent use.
type Glob struct {
	alts []alt
	all  bool // the pattern is exactly `**`
}

// Compile parses glob.
func Compile(glob string, opts Options) (*Glob, error) {
	toks, err := parse(glob, opts)
	if err != nil {
		return nil, err
	}
	if len(toks) == 1 && toks[0].kind == tRecPrefix {
		return &Glob{all: true}, nil
	}
	seqs, err := expandAlts(toks, maxAlternatives)
	if err != nil {
		return nil, err
	}
	g := &Glob{alts: make([]alt, len(seqs))}
	for i, seq := range seqs {
		g.alts[i] = compileAlt(seq)
	}
	return g, nil
}

// Union is one glob matching whatever any of globs matches.
func Union(globs ...*Glob) *Glob {
	out := &Glob{}
	for _, g := range globs {
		out.all = out.all || g.all
		out.alts = append(out.alts, g.alts...)
	}
	if out.all {
		out.alts = nil
	}
	return out
}

// Match reports whether path matches.
func (g *Glob) Match(path string) bool {
	return g.MatchBase(path, path[strings.LastIndexByte(path, '/')+1:])
}

// MatchBase is [Glob.Match] for callers that already split off base, the
// final component of path.
func (g *Glob) MatchBase(path, base string) bool {
	if g.all {
		return true
	}
	for i := range g.alts {
		if g.alts[i].match(path, base) {
			return true
		}
	}
	return false
}

// BasenameOnly reports whether matching looks only at the final path
// component, so callers may pass just that component as path.
func (g *Glob) BasenameOnly() bool {
	if g.all {
		return true
	}
	for i := range g.alts {
		if g.alts[i].kind > kBaseGlob {
			return false
		}
	}
	return true
}

// MayMatchUnder reports whether some path inside directory dir could match;
// when false, a walker can skip the whole subtree. It is exact for literal
// prefixes and conservative (true) whenever wildcards could reach below dir.
func (g *Glob) MayMatchUnder(dir string) bool {
	if g.all || dir == "" {
		return true
	}
	under := dir + "/"
	for i := range g.alts {
		a := &g.alts[i]
		var toks []token
		switch a.kind {
		case kBaseLit, kBaseSuffix, kBasePrefix, kBaseGlob:
			return true // `**/…` reaches every directory
		case kPathUnder:
			if strings.HasPrefix(under, a.lit) || strings.HasPrefix(a.lit, under) {
				return true
			}
			continue
		case kPathLit:
			toks = []token{{kind: tLit, lit: a.lit}}
		default:
			toks = a.toks
		}
		m := matcher{toks: toks, s: under, prefix: true}
		if m.at(0, 0) {
			return true
		}
	}
	return false
}

// MaxDepth is the most '/'-separated components a matching path can have,
// or 0 when a `**` or a negated class makes it unbounded.
func (g *Glob) MaxDepth() int {
	if g.all {
		return 0
	}
	depth := 0
	for i := range g.alts {
		d := g.alts[i].maxDepth()
		if d == 0 {
			return 0
		}
		depth = max(depth, d)
	}
	return depth
}

type tokKind uint8

const (
	tLit tokKind = iota
	tAny
	tStar
	tRecPrefix // leading **/
	tRecSuffix // trailing /**
	tRecMiddle // /**/
	tClass
	tAlt
)

type token struct {
	kind   tokKind
	lit    string    // tLit
	lo, hi int       // tLit while parsing: byte span of lit in the glob
	neg    bool      // tClass
	set    *byteSet  // tClass
	alts   [][]token // tAlt
}

type byteSet [4]uint64

func (s *byteSet) add(b byte)      { s[b>>6] |= 1 << (b & 63) }
func (s *byteSet) has(b byte) bool { return s[b>>6]&(1<<(b&63)) != 0 }
func (s *byteSet) addRange(lo, hi byte) {
	for c := int(lo); c <= int(hi); c++ {
		s.add(byte(c))
	}
}

func (t *token) classMatches(b byte) bool { return t.set.has(b) != t.neg }

const noRune rune = -1

// parser ports globset's Parser, including its handling of `**` inside
// alternates and its rollback of unclosed classes to a literal '['. Literal
// runs become one token spanning the glob string, so parsing allocates per
// token, not per character.
type parser struct {
	s             string
	opts          Options
	pos           int // byte offset of the next character
	curAt         int // byte offset of cur
	prev, cur     rune
	branches      [][]token
	altStack      []int
	unclosedClass bool
}

func parse(glob string, opts Options) ([]token, error) {
	p := parser{s: glob, opts: opts, prev: noRune, cur: noRune, branches: [][]token{nil}}
	for c := p.bump(); c != noRune; c = p.bump() {
		var err error
		switch {
		case c == '?':
			p.push(token{kind: tAny})
		case c == '*':
			p.parseStar()
		case c == '[' && !p.unclosedClass:
			err = p.parseClass()
		case c == '{':
			p.altStack = append(p.altStack, len(p.branches))
			p.branches = append(p.branches, nil)
		case c == '}':
			if len(p.altStack) == 0 {
				return nil, ErrInvalid
			}
			start := p.altStack[len(p.altStack)-1]
			p.altStack = p.altStack[:len(p.altStack)-1]
			alts := append([][]token(nil), p.branches[start:]...)
			p.branches = p.branches[:start]
			p.push(token{kind: tAlt, alts: alts})
		case c == ',':
			if len(p.altStack) == 0 {
				p.pushLit()
			} else {
				p.branches = append(p.branches, nil)
			}
		case c == '\\':
			if p.bump() == noRune {
				return nil, ErrInvalid
			}
			p.pushLit()
		default:
			p.pushLit()
		}
		if err != nil {
			return nil, err
		}
	}
	if len(p.branches) != 1 {
		return nil, ErrInvalid
	}
	toks := p.branches[0]
	setLits(toks, glob)
	return toks, nil
}

func setLits(toks []token, s string) {
	for i := range toks {
		switch toks[i].kind {
		case tLit:
			toks[i].lit = s[toks[i].lo:toks[i].hi]
		case tAlt:
			for _, alt := range toks[i].alts {
				setLits(alt, s)
			}
		}
	}
}

func (p *parser) bump() rune {
	p.prev = p.cur
	p.curAt = p.pos
	if p.pos < len(p.s) {
		r, n := utf8.DecodeRuneInString(p.s[p.pos:])
		p.cur = r
		p.pos += n
	} else {
		p.cur = noRune
	}
	return p.cur
}

func (p *parser) peek() rune {
	if p.pos < len(p.s) {
		r, _ := utf8.DecodeRuneInString(p.s[p.pos:])
		return r
	}
	return noRune
}

func (p *parser) push(t token) {
	last := len(p.branches) - 1
	p.branches[last] = append(p.branches[last], t)
}

// pushLit pushes the current character as a literal, extending the previous
// literal when it ends right where this character starts.
func (p *parser) pushLit() {
	b := p.branches[len(p.branches)-1]
	if n := len(b); n > 0 && b[n-1].kind == tLit && b[n-1].hi == p.curAt {
		b[n-1].hi = p.pos
		return
	}
	p.push(token{kind: tLit, lo: p.curAt, hi: p.pos})
}

// pop removes the last token as globset's pop_token would. Literal runs are
// one token here, so a literal loses only its final character.
func (p *parser) pop() token {
	last := len(p.branches) - 1
	b := p.branches[last]
	t := b[len(b)-1]
	if t.kind == tLit {
		_, n := utf8.DecodeLastRuneInString(p.s[t.lo:t.hi])
		if b[len(b)-1].hi -= n; b[len(b)-1].hi > t.lo {
			return token{kind: tLit}
		}
	}
	p.branches[last] = b[:len(b)-1]
	return t
}

func (p *parser) parseStar() {
	prev := p.prev
	star := token{kind: tStar}
	if p.peek() != '*' {
		p.push(star)
		return
	}
	p.bump()
	if len(p.branches[len(p.branches)-1]) == 0 {
		if next := p.peek(); next != noRune && next != '/' {
			p.push(star)
			p.push(star)
		} else {
			p.push(token{kind: tRecPrefix})
			p.bump()
		}
		return
	}
	if prev != '/' && (len(p.branches) <= 1 || (prev != ',' && prev != '{')) {
		p.push(star)
		p.push(star)
		return
	}
	var suffix bool
	switch next := p.peek(); {
	case next == noRune:
		p.bump()
		suffix = true
	case (next == ',' || next == '}') && len(p.branches) >= 2:
		suffix = true
	case next == '/':
		p.bump()
	default:
		p.push(star)
		p.push(star)
		return
	}
	switch prevTok := p.pop(); prevTok.kind {
	case tRecPrefix, tRecSuffix:
		p.push(prevTok)
	default:
		if suffix {
			p.push(token{kind: tRecSuffix})
		} else {
			p.push(token{kind: tRecMiddle})
		}
	}
}

func (p *parser) parseClass() error {
	savedPos, savedAt, savedPrev, savedCur := p.pos, p.curAt, p.prev, p.cur
	type span struct{ lo, hi rune }
	var ranges []span
	neg := false
	if next := p.peek(); next == '!' || next == '^' {
		p.bump()
		neg = true
	}
	first, inRange := true, false
	for {
		c := p.bump()
		if c == noRune {
			if !p.opts.AllowUnclosedClass {
				return ErrInvalid
			}
			p.pos, p.curAt, p.prev, p.cur = savedPos, savedAt, savedPrev, savedCur
			p.unclosedClass = true
			p.pushLit()
			return nil
		}
		switch {
		case c == ']' && !first:
			if inRange {
				ranges = append(ranges, span{'-', '-'})
			}
			set := new(byteSet)
			for _, r := range ranges {
				addClassSpan(set, r.lo, r.hi)
			}
			p.push(token{kind: tClass, neg: neg, set: set})
			return nil
		case c == '-' && first:
			ranges = append(ranges, span{'-', '-'})
		case c == '-' && inRange:
			r := &ranges[len(ranges)-1]
			if r.hi = '-'; r.hi < r.lo {
				return ErrInvalid
			}
			inRange = false
		case c == '-':
			inRange = true
		case inRange:
			r := &ranges[len(ranges)-1]
			if r.hi = c; r.hi < r.lo {
				return ErrInvalid
			}
			inRange = false
		default:
			ranges = append(ranges, span{c, c})
		}
		first = false
	}
}

// addClassSpan adds a character range the way globset's byte-oriented regex
// sees it: each character becomes its UTF-8 bytes, and a range spans only the
// last byte of lo to the first byte of hi. ASCII ranges are exact.
func addClassSpan(set *byteSet, lo, hi rune) {
	var a, b [utf8.UTFMax]byte
	la := utf8.EncodeRune(a[:], lo)
	if lo == hi {
		for _, c := range a[:la] {
			set.add(c)
		}
		return
	}
	lb := utf8.EncodeRune(b[:], hi)
	for _, c := range a[:la-1] {
		set.add(c)
	}
	if a[la-1] <= b[0] {
		set.addRange(a[la-1], b[0])
	}
	for _, c := range b[1:lb] {
		set.add(c)
	}
}

// expandAlts flattens alternates into plain token sequences, merging
// adjacent literals. Empty alternatives are dropped as in globset; an
// alternate whose every branch is empty matches the empty string.
func expandAlts(toks []token, limit int) ([][]token, error) {
	if !slices.ContainsFunc(toks, func(t token) bool { return t.kind == tAlt }) {
		return [][]token{toks}, nil
	}
	seqs := [][]token{nil}
	for _, t := range toks {
		if t.kind != tAlt {
			for i := range seqs {
				seqs[i] = appendTok(seqs[i], t)
			}
			continue
		}
		var branches [][]token
		for _, alt := range t.alts {
			if len(alt) == 0 {
				continue
			}
			sub, err := expandAlts(alt, limit)
			if err != nil {
				return nil, err
			}
			branches = append(branches, sub...)
		}
		if len(branches) == 0 {
			continue
		}
		if len(seqs)*len(branches) > limit {
			return nil, ErrInvalid
		}
		next := make([][]token, 0, len(seqs)*len(branches))
		for _, s := range seqs {
			for _, b := range branches {
				seq := append([]token(nil), s...)
				for _, bt := range b {
					seq = appendTok(seq, bt)
				}
				next = append(next, seq)
			}
		}
		seqs = next
	}
	return seqs, nil
}

func appendTok(seq []token, t token) []token {
	if t.kind == tLit && len(seq) > 0 && seq[len(seq)-1].kind == tLit {
		seq[len(seq)-1].lit += t.lit
		return seq
	}
	return append(seq, t)
}

// altKind selects a specialised matcher for the common shapes.
type altKind uint8

const (
	kBaseLit    altKind = iota // basename == lit
	kBaseSuffix                // basename ends with lit (`*.log`)
	kBasePrefix                // basename starts with lit (`foo*`)
	kBaseGlob                  // tokens against the basename
	kPathLit                   // path == lit
	kPathUnder                 // path has prefix lit (lit ends in '/'; `dir/**`)
	kPathGlob                  // tokens against the whole path
)

// alt is one alternative of a compiled glob.
type alt struct {
	kind altKind
	lit  string
	toks []token
	memo bool // backtracking could go exponential; memoize failures
}

func compileAlt(toks []token) alt {
	if len(toks) > 1 && toks[0].kind == tRecPrefix && !canMatchSlash(toks[1:]) {
		rest := toks[1:]
		switch {
		case len(rest) == 1 && rest[0].kind == tLit:
			return alt{kind: kBaseLit, lit: rest[0].lit}
		case len(rest) == 2 && rest[0].kind == tStar && rest[1].kind == tLit:
			return alt{kind: kBaseSuffix, lit: rest[1].lit}
		case len(rest) == 2 && rest[0].kind == tLit && rest[1].kind == tStar:
			return alt{kind: kBasePrefix, lit: rest[0].lit}
		}
		return alt{kind: kBaseGlob, toks: rest, memo: needsMemo(rest)}
	}
	switch {
	case len(toks) == 1 && toks[0].kind == tLit:
		return alt{kind: kPathLit, lit: toks[0].lit}
	case len(toks) == 3 && toks[0].kind == tLit && toks[1].kind == tRecMiddle && toks[2].kind == tStar:
		return alt{kind: kPathUnder, lit: toks[0].lit + "/"}
	}
	return alt{kind: kPathGlob, toks: toks, memo: needsMemo(toks)}
}

// canMatchSlash reports whether toks could consume a '/', which would let a
// `**/`-prefixed alternative match more than the final path component.
func canMatchSlash(toks []token) bool {
	for i := range toks {
		t := &toks[i]
		switch t.kind {
		case tLit:
			if strings.IndexByte(t.lit, '/') >= 0 {
				return true
			}
		case tClass:
			if t.classMatches('/') {
				return true
			}
		case tRecPrefix, tRecSuffix, tRecMiddle:
			return true
		}
	}
	return false
}

func needsMemo(toks []token) bool {
	n := 0
	for _, t := range toks {
		if t.kind >= tStar && t.kind <= tRecMiddle {
			n++
		}
	}
	return n > 2
}

func (a *alt) maxDepth() int {
	switch a.kind {
	case kPathLit:
		return strings.Count(a.lit, "/") + 1
	case kPathGlob:
		depth := 1
		for i := range a.toks {
			t := &a.toks[i]
			switch t.kind {
			case tLit:
				depth += strings.Count(t.lit, "/")
			case tClass:
				if t.classMatches('/') {
					return 0
				}
			case tRecPrefix, tRecSuffix, tRecMiddle:
				return 0
			}
		}
		return depth
	}
	return 0
}

func (a *alt) match(path, base string) bool {
	switch a.kind {
	case kBaseLit:
		return base == a.lit
	case kBaseSuffix:
		return strings.HasSuffix(base, a.lit)
	case kBasePrefix:
		return strings.HasPrefix(base, a.lit)
	case kBaseGlob:
		return matchToks(a.toks, base, a.memo)
	case kPathLit:
		return path == a.lit
	case kPathUnder:
		return len(path) > len(a.lit) && strings.HasPrefix(path, a.lit)
	}
	return matchToks(a.toks, path, a.memo)
}

func matchToks(toks []token, s string, memo bool) bool {
	m := matcher{toks: toks, s: s}
	if memo {
		m.failed = make([]uint64, ((len(toks)+1)*(len(s)+1)+63)/64)
	}
	return m.at(0, 0)
}

// matcher is a backtracking matcher over one token sequence. With failed
// set, each (token, offset) state is explored at most once, bounding the
// work by tokens×bytes for patterns with many wildcards. With prefix set it
// instead reports whether s could be extended into a match: running out of
// input anywhere inside the pattern counts as success.
type matcher struct {
	toks   []token
	s      string
	failed []uint64
	prefix bool
}

func (m *matcher) at(ti, si int) bool {
	if m.failed != nil {
		bit := ti*(len(m.s)+1) + si
		if m.failed[bit>>6]&(1<<(bit&63)) != 0 {
			return false
		}
		if m.step(ti, si) {
			return true
		}
		m.failed[bit>>6] |= 1 << (bit & 63)
		return false
	}
	return m.step(ti, si)
}

func (m *matcher) step(ti, si int) bool {
	s := m.s
	for ti < len(m.toks) {
		if m.prefix && si == len(s) {
			return true
		}
		t := &m.toks[ti]
		switch t.kind {
		case tLit:
			if m.prefix && len(s)-si < len(t.lit) {
				return strings.HasPrefix(t.lit, s[si:])
			}
			if !strings.HasPrefix(s[si:], t.lit) {
				return false
			}
			si += len(t.lit)
		case tAny:
			if si == len(s) || s[si] == '/' {
				return false
			}
			si++
		case tClass:
			if si == len(s) || !t.classMatches(s[si]) {
				return false
			}
			si++
		case tStar:
			if ti+1 == len(m.toks) && !m.prefix {
				return strings.IndexByte(s[si:], '/') < 0
			}
			for ; ; si++ {
				if m.at(ti+1, si) {
					return true
				}
				if si == len(s) || s[si] == '/' {
					return false
				}
			}
		case tRecPrefix:
			// (?:/?|.*/)
			if m.at(ti+1, si) {
				return true
			}
			for i := si; i < len(s); i++ {
				if s[i] == '/' && m.at(ti+1, i+1) {
					return true
				}
			}
			return m.prefix
		case tRecSuffix:
			// /.*
			if si == len(s) || s[si] != '/' {
				return false
			}
			for i := si + 1; i <= len(s); i++ {
				if m.at(ti+1, i) {
					return true
				}
			}
			return false
		case tRecMiddle:
			// (?:/|/.*/)
			if si == len(s) || s[si] != '/' {
				return false
			}
			if m.prefix {
				return true
			}
			for i := si; i < len(s); i++ {
				if s[i] == '/' && m.at(ti+1, i+1) {
					return true
				}
			}
			return false
		}
		ti++
	}
	return si == len(s)
}
