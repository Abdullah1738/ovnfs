package grep

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// This file turns a pattern written for Rust's regex crate into a Go
// regexp with the same meaning where Go can express it.
//
// Rust semantics kept:
//   - \d \w \s and their negations are Unicode (\p{Nd}; Alphabetic, marks,
//     digits, connector punctuation and Join_Control; White_Space), not
//     Go's ASCII classes.
//   - (?i) is Unicode simple case folding (Go's folding is the same).
//   - \u{…} \uXXXX \U{…} \UXXXXXXXX escapes.
//   - Compatibility repairs: braces that are not repetitions are literal ("${x}"),
//     a stray '(' or ')' is escaped, and a pattern no engine accepts is
//     searched literally.
//   - Outside multiline mode a match never spans lines: '\n' is removed from
//     every class and a literal '\n' cannot match (grep-regex's line
//     terminator rule); ^ and $ anchor at line boundaries.
//
// Known differences (Go cannot express them):
//   - \b and \B are ASCII word boundaries ("éfoo" matches \bfoo here, not in
//     Rust).
//   - PCRE-only syntax (lookaround, backrefs,
//     atomic groups, possessive quantifiers) and Rust-only syntax (nested
//     classes and class set operations, \< \> \b{…}, the x and u flags,
//     binary Unicode properties such as \p{Alphabetic}) are rejected with
//     an error instead of being misread as a literal.

// compiled is a pattern ready for scanning: the regexp (multiline mode
// locates matches with it), the program line mode runs as a DFA (dfa.go),
// and what the literal analysis of literal.go derived.
type compiled struct {
	never     bool           // no line can match (line mode, literal '\n')
	re        *regexp.Regexp // multiline mode only
	prog      *syntax.Prog   // line mode only
	pre       *prefilter     // nil: no literal every match must contain
	exact     bool           // line mode: a prefilter hit is a match
	multiline bool
	// locate returns the regexp that finds match positions within a line or
	// block (windowing cut lines, see window.go): re in multiline mode,
	// otherwise compiled on first use from the line-mode tree; nil if that
	// fails (cut lines then keep their head).
	locate func() *regexp.Regexp
}

// compilePattern builds the matcher for pattern. literal searches for the
// pattern text itself; multiline lets matches span lines.
func compilePattern(pat string, literal, ignoreCase, multiline bool) (*compiled, error) {
	flags := syntax.ClassNL | syntax.PerlX | syntax.UnicodeGroups
	if ignoreCase {
		flags |= syntax.FoldCase
	}
	var re *syntax.Regexp
	var err error
	if literal {
		re, err = syntax.Parse(regexp.QuoteMeta(pat), flags)
	} else {
		re, err = parseRust(pat, flags)
	}
	if err != nil {
		return nil, err
	}
	if !multiline {
		re = lineMode(re)
	}
	re = re.Simplify()
	c := &compiled{multiline: multiline, never: re.Op == syntax.OpNoMatch}
	if c.never {
		return c, nil
	}
	// Both compilers accept every tree the parser produces; failing here is
	// a bug, not bad input.
	if multiline {
		c.re, err = regexp.Compile(re.String())
		c.locate = func() *regexp.Regexp { return c.re }
	} else {
		c.prog, err = syntax.Compile(re)
		src := re.String()
		c.locate = sync.OnceValue(func() *regexp.Regexp {
			loc, err := regexp.Compile(src)
			if err != nil {
				return nil
			}
			return loc
		})
	}
	if err != nil {
		return nil, fmt.Errorf("grep: internal regexp error for %q: %w", pat, err)
	}
	c.pre, c.exact = buildPrefilter(re)
	c.exact = c.exact && !multiline
	return c, nil
}

// parseRust parses a Rust-dialect pattern with compatibility repairs (see
// build_matcher in grep.rs): sanitised braces, then escaped parentheses for
// a group error, then the literal text.
func parseRust(pat string, flags syntax.Flags) (*syntax.Regexp, error) {
	src := sanitizeBraces(pat)
	// Checked first: Go reads some of these constructs (\<, [a&&b]) as
	// literals instead of rejecting them.
	if what := unsupported(src); what != "" {
		return nil, fmt.Errorf("grep: pattern %q uses %s, which the regexp engine does not support", pat, what)
	}
	re, err := syntax.Parse(translate(src), flags)
	if err == nil {
		return re, nil
	}
	var serr *syntax.Error
	if errors.As(err, &serr) && (serr.Code == syntax.ErrMissingParen || serr.Code == syntax.ErrUnexpectedParen) {
		if esc := escapeParens(src); esc != src {
			if re, err := syntax.Parse(translate(esc), flags); err == nil {
				return re, nil
			}
		}
	}
	return syntax.Parse(regexp.QuoteMeta(pat), flags)
}

// lineMode adapts a tree to line-at-a-time matching, as grep-regex does with
// a line terminator: no class or dot matches '\n', a literal containing
// '\n' never matches, and text anchors are line anchors (each line is the
// whole text).
func lineMode(re *syntax.Regexp) *syntax.Regexp {
	switch re.Op {
	case syntax.OpLiteral:
		if slices.Contains(re.Rune, '\n') {
			return &syntax.Regexp{Op: syntax.OpNoMatch}
		}
	case syntax.OpCharClass:
		re.Rune = withoutNL(re.Rune)
		if len(re.Rune) == 0 {
			return &syntax.Regexp{Op: syntax.OpNoMatch}
		}
	case syntax.OpAnyChar:
		re.Op, re.Rune = syntax.OpAnyCharNotNL, nil
	case syntax.OpBeginText:
		re.Op = syntax.OpBeginLine
	case syntax.OpEndText:
		re.Op, re.Flags = syntax.OpEndLine, re.Flags&^syntax.WasDollar
	}
	for i, sub := range re.Sub {
		re.Sub[i] = lineMode(sub)
	}
	return re
}

// withoutNL removes '\n' from a class's sorted [lo, hi] range pairs.
func withoutNL(r []rune) []rune {
	out := r[:0:0]
	for i := 0; i+1 < len(r); i += 2 {
		lo, hi := r[i], r[i+1]
		if lo <= '\n' && '\n' <= hi {
			if lo < '\n' {
				out = append(out, lo, '\n'-1)
			}
			if hi > '\n' {
				out = append(out, '\n'+1, hi)
			}
			continue
		}
		out = append(out, lo, hi)
	}
	return out
}

// sanitizeBraces repairs '{' and '}' that do not form a
// repetition ({n}, {n,}, {n,m}) or a braced escape (\p{…} \x{…} \u{…}) are
// escaped, so literal text such as "${platform}" parses.
func sanitizeBraces(p string) string {
	if !strings.ContainsAny(p, "{}") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); {
		c := p[i]
		if c == '\\' && i+1 < len(p) {
			_, n := utf8.DecodeRuneInString(p[i+1:])
			e := p[i+1]
			b.WriteString(p[i : i+1+n])
			i += 1 + n
			if (e == 'p' || e == 'P' || e == 'x' || e == 'u') && i < len(p) && p[i] == '{' {
				end := strings.IndexByte(p[i:], '}')
				if end < 0 {
					b.WriteString(p[i:])
					i = len(p)
				} else {
					b.WriteString(p[i : i+end+1])
					i += end + 1
				}
			}
			continue
		}
		switch c {
		case '{':
			if end := repetitionEnd(p, i); end > 0 {
				b.WriteString(p[i : end+1])
				i = end + 1
				continue
			}
			b.WriteString(`\{`)
		case '}':
			b.WriteString(`\}`)
		default:
			b.WriteByte(c)
		}
		i++
	}
	return b.String()
}

// repetitionEnd returns the index of the '}' closing a repetition that
// starts at p[i] == '{', or -1.
func repetitionEnd(p string, i int) int {
	j := i + 1
	digits := func() int {
		k := j
		for j < len(p) && '0' <= p[j] && p[j] <= '9' {
			j++
		}
		return j - k
	}
	if digits() == 0 || j >= len(p) {
		return -1
	}
	if p[j] == '}' {
		return j
	}
	if p[j] != ',' {
		return -1
	}
	j++
	digits()
	if j < len(p) && p[j] == '}' {
		return j
	}
	return -1
}

// escapeParens makes every unescaped '(' and ')' literal, for snippets
// such as "fetchProvider(".
func escapeParens(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '\\' && i+1 < len(p):
			b.WriteByte(c)
			i++
			b.WriteByte(p[i])
		case c == '(' || c == ')':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// unsupported names the first construct in p that Rust or PCRE reads as
// syntax but Go's regexp cannot express (rejecting it or misreading it as a
// literal), or returns "".
func unsupported(p string) string {
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		rest := p[i:]
		if c == '\\' && i+1 < len(p) {
			switch e := p[i+1]; {
			case '1' <= e && e <= '9', e == 'k', e == 'g':
				return "a backreference"
			case e == 'K' || e == 'G' || e == 'R' || e == 'X':
				return "a PCRE escape (\\" + string(e) + ")"
			case e == '<' || e == '>' || strings.HasPrefix(rest, `\b{`) || strings.HasPrefix(rest, `\B{`):
				return "a word-boundary assertion other than \\b"
			}
			i++
			continue
		}
		if inClass {
			switch {
			case c == ']':
				inClass = false
			case strings.HasPrefix(rest, "[:"):
				if end := strings.Index(rest, ":]"); end > 0 {
					i += end + 1
				}
			case c == '[':
				return "a nested character class"
			case strings.HasPrefix(rest, "&&"), strings.HasPrefix(rest, "--"), strings.HasPrefix(rest, "~~"):
				return "a character class set operation"
			}
			continue
		}
		switch {
		case c == '[':
			inClass = true
			if strings.HasPrefix(rest, "[^]") {
				i += 2
			} else if strings.HasPrefix(rest, "[]") {
				i++
			}
		case strings.HasPrefix(rest, "(?="), strings.HasPrefix(rest, "(?!"),
			strings.HasPrefix(rest, "(?<="), strings.HasPrefix(rest, "(?<!"):
			return "lookaround"
		case strings.HasPrefix(rest, "(?>"), strings.HasPrefix(rest, "(?|"), strings.HasPrefix(rest, "(?("),
			strings.HasPrefix(rest, "(?R"), strings.HasPrefix(rest, "(*"):
			return "a PCRE group construct"
		case strings.HasPrefix(rest, "(?"):
			flags := rest[2:]
			if end := strings.IndexAny(flags, ":)"); end >= 0 && strings.ContainsAny(flags[:end], "xuR") &&
				strings.Trim(flags[:end], "imsUxuR-") == "" {
				return "the x, u, or R flag"
			}
		case (c == '+' || c == '*' || c == '?' || c == '}') && i+1 < len(p) && p[i+1] == '+':
			return "a possessive quantifier"
		}
	}
	return ""
}

// translate rewrites Rust escapes whose Go spelling differs: Unicode
// \d \w \s (and negations), \u and \U code points. It tracks character
// classes so negated classes inside brackets stay correct.
func translate(p string) string {
	if !strings.ContainsRune(p, '\\') {
		return p
	}
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c != '\\' || i+1 >= len(p) {
			if inClass {
				switch {
				case c == ']':
					inClass = false
				case strings.HasPrefix(p[i:], "[:"):
					if end := strings.Index(p[i:], ":]"); end > 0 {
						b.WriteString(p[i : i+end+2])
						i += end + 1
						continue
					}
				}
			} else if c == '[' {
				inClass = true
				b.WriteByte(c)
				switch {
				case strings.HasPrefix(p[i:], "[^]"):
					b.WriteString("^]")
					i += 2
				case strings.HasPrefix(p[i:], "[]"):
					b.WriteByte(']')
					i++
				}
				continue
			}
			b.WriteByte(c)
			continue
		}
		e := p[i+1]
		i++
		switch e {
		case 'd':
			b.WriteString(`\p{Nd}`)
		case 'D':
			b.WriteString(`\P{Nd}`)
		case 'w', 'W', 's', 'S':
			set := classWord
			if e == 's' || e == 'S' {
				set = classSpace
			}
			neg := e == 'W' || e == 'S'
			switch {
			case inClass && neg:
				b.WriteString(set.negItems)
			case inClass:
				b.WriteString(set.items)
			case neg:
				b.WriteString("[^" + set.items + "]")
			default:
				b.WriteString("[" + set.items + "]")
			}
		case 'u', 'U':
			if r, n, ok := codePoint(p[i+1:], e); ok {
				fmt.Fprintf(&b, `\x{%x}`, r)
				i += n
			} else {
				b.WriteByte('\\')
				b.WriteByte(e)
			}
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
		}
	}
	return b.String()
}

// codePoint parses the operand of a Rust \u or \U escape (the text after
// the letter): {hex} or exactly 4 (\u) or 8 (\U) hex digits. It returns the
// rune and the operand's length.
func codePoint(s string, kind byte) (rune, int, bool) {
	var hex string
	n := 0
	if strings.HasPrefix(s, "{") {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return 0, 0, false
		}
		hex, n = s[1:end], end+1
	} else {
		n = 4
		if kind == 'U' {
			n = 8
		}
		if len(s) < n {
			return 0, 0, false
		}
		hex = s[:n]
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil || v > unicode.MaxRune {
		return 0, 0, false
	}
	return rune(v), n, true
}

// perlClass holds a Unicode Perl class in Go class-item syntax: items for
// use inside brackets, negItems for its complement inside brackets.
type perlClass struct {
	items, negItems string
}

var (
	// classWord is Rust's Unicode \w: Alphabetic, M, Nd, Pc, Join_Control.
	// Alphabetic is L, Nl, and Other_Alphabetic; Go names the categories but
	// not the two properties, so their remainder is spelled as ranges.
	classWord = func() perlClass {
		cats := []*unicode.RangeTable{unicode.L, unicode.M, unicode.Nd, unicode.Nl, unicode.Pc}
		all := unionRanges(append(cats, unicode.Other_Alphabetic, unicode.Join_Control)...)
		extra := subtractRanges(all, unionRanges(cats...))
		return perlClass{
			items:    `\p{L}\p{M}\p{Nd}\p{Nl}\p{Pc}` + rangeItems(extra),
			negItems: rangeItems(complementRanges(all)),
		}
	}()
	// classSpace is Rust's Unicode \s: the White_Space property.
	classSpace = func() perlClass {
		ws := unionRanges(unicode.White_Space)
		return perlClass{items: rangeItems(ws), negItems: rangeItems(complementRanges(ws))}
	}()
)

// unionRanges merges tables into sorted, disjoint, non-adjacent [lo, hi]
// pairs.
func unionRanges(tabs ...*unicode.RangeTable) [][2]rune {
	var rs [][2]rune
	for _, t := range tabs {
		for _, r := range t.R16 {
			for lo := rune(r.Lo); lo <= rune(r.Hi); lo += rune(r.Stride) {
				hi := lo
				if r.Stride == 1 {
					hi = rune(r.Hi)
				}
				rs = append(rs, [2]rune{lo, hi})
				if r.Stride == 1 {
					break
				}
			}
		}
		for _, r := range t.R32 {
			for lo := rune(r.Lo); lo <= rune(r.Hi); lo += rune(r.Stride) {
				hi := lo
				if r.Stride == 1 {
					hi = rune(r.Hi)
				}
				rs = append(rs, [2]rune{lo, hi})
				if r.Stride == 1 {
					break
				}
			}
		}
	}
	slices.SortFunc(rs, func(a, b [2]rune) int { return int(a[0] - b[0]) })
	out := rs[:0]
	for _, r := range rs {
		if n := len(out); n > 0 && r[0] <= out[n-1][1]+1 {
			out[n-1][1] = max(out[n-1][1], r[1])
			continue
		}
		out = append(out, r)
	}
	return out
}

// complementRanges is every code point not in rs.
func complementRanges(rs [][2]rune) [][2]rune {
	var out [][2]rune
	next := rune(0)
	for _, r := range rs {
		if r[0] > next {
			out = append(out, [2]rune{next, r[0] - 1})
		}
		next = r[1] + 1
	}
	if next <= unicode.MaxRune {
		out = append(out, [2]rune{next, unicode.MaxRune})
	}
	return out
}

// subtractRanges is a minus b.
func subtractRanges(a, b [][2]rune) [][2]rune {
	var out [][2]rune
	for _, r := range a {
		lo := r[0]
		for _, s := range b {
			if s[1] < lo || s[0] > r[1] {
				continue
			}
			if s[0] > lo {
				out = append(out, [2]rune{lo, s[0] - 1})
			}
			lo = s[1] + 1
		}
		if lo <= r[1] {
			out = append(out, [2]rune{lo, r[1]})
		}
	}
	return out
}

// rangeItems spells ranges as class items (\x{lo}-\x{hi}).
func rangeItems(rs [][2]rune) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, `\x{%x}`, r[0])
		if r[1] != r[0] {
			fmt.Fprintf(&b, `-\x{%x}`, r[1])
		}
	}
	return b.String()
}
