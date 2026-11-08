package config

import "strings"

// globMatch reports whether name matches pattern under git's wildmatch rules
// with pathname semantics (gitignore(5) "PATTERN FORMAT"): '*' and '?' never
// match '/', "**/" at the start or after a '/' matches zero or more
// directories, a trailing "/**" matches everything below, "[...]" is a
// bracket expression, '\' quotes the next character, and other runs of '*'
// act as one. fold compares ASCII letters case-insensitively (gitdir/i:).
func globMatch(pattern, name string, fold bool) bool {
	return glob{pattern, name, fold}.match(0, 0)
}

type glob struct {
	p, t string
	fold bool
}

func (g glob) match(pi, ti int) bool {
	p, t := g.p, g.t
	for pi < len(p) {
		switch c := p[pi]; c {
		case '\\':
			// A trailing backslash is an invalid pattern that never matches.
			if pi+1 == len(p) || ti == len(t) || !g.same(p[pi+1], t[ti]) {
				return false
			}
			pi, ti = pi+2, ti+1
		case '?':
			if ti == len(t) || t[ti] == '/' {
				return false
			}
			pi, ti = pi+1, ti+1
		case '[':
			if ti == len(t) || t[ti] == '/' {
				return false
			}
			n, ok := g.bracket(pi, t[ti])
			if !ok {
				return false
			}
			pi, ti = n, ti+1
		case '*':
			return g.stars(pi, ti)
		default:
			if ti == len(t) || !g.same(c, t[ti]) {
				return false
			}
			pi, ti = pi+1, ti+1
		}
	}
	return ti == len(t)
}

// stars matches the run of '*' starting at p[pi] and the rest of the pattern.
func (g glob) stars(pi, ti int) bool {
	p, t := g.p, g.t
	start := pi
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	segment := (start == 0 || p[start-1] == '/') && (pi == len(p) || p[pi] == '/')
	if pi-start >= 2 && segment {
		if pi == len(p) {
			return true // trailing "**" (after '/' or alone) matches everything
		}
		pi++ // "**/": zero or more whole directories
		for {
			if g.match(pi, ti) {
				return true
			}
			i := strings.IndexByte(t[ti:], '/')
			if i < 0 {
				return false
			}
			ti += i + 1
		}
	}
	for ; ; ti++ {
		if g.match(pi, ti) {
			return true
		}
		if ti == len(t) || t[ti] == '/' {
			return false
		}
	}
}

// bracket matches c against the bracket expression at p[pi] ('['), returning
// the index after its closing ']'. An unterminated expression never matches.
func (g glob) bracket(pi int, c byte) (int, bool) {
	p := g.p
	i := pi + 1
	negate := i < len(p) && (p[i] == '!' || p[i] == '^')
	if negate {
		i++
	}
	matched := false
	for first := true; ; first = false {
		if i >= len(p) {
			return 0, false
		}
		lo := p[i]
		if lo == ']' && !first {
			i++
			break
		}
		if lo == '[' && i+1 < len(p) && p[i+1] == ':' {
			end := strings.Index(p[i+2:], ":]")
			if end < 0 {
				return 0, false
			}
			class := p[i+2 : i+2+end]
			ok, known := inClass(class, c, g.fold)
			if !known {
				return 0, false
			}
			matched = matched || ok
			i += end + 4
			continue
		}
		if lo == '\\' {
			i++
			if i >= len(p) {
				return 0, false
			}
			lo = p[i]
		}
		i++
		hi := lo
		if i+1 < len(p) && p[i] == '-' && p[i+1] != ']' {
			hi = p[i+1]
			i += 2
			if hi == '\\' {
				if i >= len(p) {
					return 0, false
				}
				hi = p[i]
				i++
			}
		}
		if lo <= c && c <= hi {
			matched = true
		} else if g.fold {
			lc := lowerByte(c)
			uc := upperByte(c)
			matched = matched || (lo <= lc && lc <= hi) || (lo <= uc && uc <= hi)
		}
	}
	return i, matched != negate
}

// inClass evaluates a POSIX character class name; known is false for names
// git does not recognise, which make the whole pattern fail.
func inClass(name string, c byte, fold bool) (ok, known bool) {
	isUpper := 'A' <= c && c <= 'Z'
	isLower := 'a' <= c && c <= 'z'
	isDigit := '0' <= c && c <= '9'
	switch name {
	case "alnum":
		return isUpper || isLower || isDigit, true
	case "alpha":
		return isUpper || isLower, true
	case "blank":
		return c == ' ' || c == '\t', true
	case "cntrl":
		return c < 0x20 || c == 0x7f, true
	case "digit":
		return isDigit, true
	case "graph":
		return c > 0x20 && c < 0x7f, true
	case "lower":
		return isLower || (fold && isUpper), true
	case "print":
		return c >= 0x20 && c < 0x7f, true
	case "punct":
		return c > 0x20 && c < 0x7f && !isUpper && !isLower && !isDigit, true
	case "space":
		return isSpace(c), true
	case "upper":
		return isUpper || (fold && isLower), true
	case "xdigit":
		return isDigit || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F'), true
	}
	return false, false
}

func (g glob) same(a, b byte) bool {
	return a == b || (g.fold && lowerByte(a) == lowerByte(b))
}

func lowerByte(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func upperByte(c byte) byte {
	if 'a' <= c && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}
