package config

import (
	"fmt"
	"strings"
)

// ParseError reports a config file git would refuse to read. Its message
// matches git's "bad config line N in file F", optionally followed by the
// specific problem.
type ParseError struct {
	File   string // the file as opened, or the name given to Parse
	Line   int    // 1-based line number, counted as git counts it
	Reason string // optional detail ("missing value for 'include.path'")
}

func (e *ParseError) Error() string {
	msg := fmt.Sprintf("bad config line %d in file %s", e.Line, e.File)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// rawEntry is one variable as written in a file, before includes.
type rawEntry struct {
	key      string // canonical: section header lower-cased (subsection kept) + "." + lower-cased name
	value    string
	hasValue bool
	line     int
}

// parse splits data, the contents of file, into variables in file order.
func parse(data []byte, file string) ([]rawEntry, error) {
	s := scanner{data: data, line: 1}
	if len(data) >= 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf {
		s.pos = 3 // a UTF-8 byte order mark is skipped at the very start only
	}
	var out []rawEntry
	var prefix string // canonical section header of the current section
	for {
		c, ok := s.next()
		if !ok {
			return out, nil
		}
		switch {
		case c == '\n':
			s.line++
		case isSpace(c):
		case c == '#' || c == ';':
			s.skipLine()
		case c == '[':
			p, ok := s.header()
			if !ok {
				return nil, &ParseError{File: file, Line: s.line}
			}
			prefix = p
		case isAlpha(c):
			line := s.line
			e, ok := s.variable(c)
			if !ok {
				return nil, &ParseError{File: file, Line: s.line}
			}
			e.line = line
			if prefix != "" {
				e.key = prefix + "." + e.key
			}
			out = append(out, e)
		default:
			return nil, &ParseError{File: file, Line: s.line}
		}
	}
}

// scanner reads config bytes, presenting CR LF as a single LF.
type scanner struct {
	data []byte
	pos  int
	line int
}

func (s *scanner) next() (byte, bool) {
	if s.pos >= len(s.data) {
		return 0, false
	}
	c := s.data[s.pos]
	s.pos++
	if c == '\r' && s.pos < len(s.data) && s.data[s.pos] == '\n' {
		s.pos++
		return '\n', true
	}
	return c, true
}

// skipLine consumes the rest of a comment, including its newline.
func (s *scanner) skipLine() {
	for {
		c, ok := s.next()
		if !ok {
			return
		}
		if c == '\n' {
			s.line++
			return
		}
	}
}

// header parses a section header after its '[' and returns the canonical
// prefix: the lower-cased name (dots kept, for the legacy [section.sub] form),
// then "." and the subsection verbatim when one is quoted.
func (s *scanner) header() (string, bool) {
	var name strings.Builder
	for {
		c, ok := s.next()
		if !ok {
			return "", false
		}
		switch {
		case c == ']':
			return name.String(), name.Len() > 0
		case isSpace(c) && c != '\n':
			if name.Len() == 0 {
				return "", false
			}
			return s.subsection(name.String())
		case isAlnum(c) || c == '-' || c == '.':
			name.WriteByte(lowerByte(c))
		default:
			return "", false
		}
	}
}

func (s *scanner) subsection(name string) (string, bool) {
	c, ok := s.next()
	for ok && isSpace(c) && c != '\n' {
		c, ok = s.next()
	}
	if !ok || c != '"' {
		return "", false
	}
	var sub strings.Builder
	for {
		c, ok := s.next()
		if !ok || c == '\n' {
			return "", false
		}
		switch c {
		case '\\':
			c, ok = s.next()
			if !ok || c == '\n' {
				return "", false
			}
			sub.WriteByte(c) // any escaped character stands for itself
		case '"':
			if c, ok = s.next(); !ok || c != ']' {
				if !ok || c == '\n' {
					s.line++ // git has already counted the line end here
				}
				return "", false
			}
			return name + "." + sub.String(), true
		default:
			sub.WriteByte(c)
		}
	}
}

// variable parses "name [= value]" starting at the name's first letter.
func (s *scanner) variable(first byte) (rawEntry, bool) {
	name := []byte{lowerByte(first)}
	c, ok := s.next()
	for ok && (isAlnum(c) || c == '-') {
		name = append(name, lowerByte(c))
		c, ok = s.next()
	}
	for ok && isSpace(c) && c != '\n' {
		c, ok = s.next()
	}
	e := rawEntry{key: string(name)}
	switch {
	case !ok:
		return e, true
	case c == '\n':
		s.line++
		return e, true
	case c != '=':
		return e, false
	}
	value, ok := s.value()
	if !ok {
		return e, false
	}
	e.value, e.hasValue = value, true
	return e, true
}

// value parses the text after '=' up to the end of its (possibly continued)
// line. Whitespace outside quotes is dropped at the start and end and kept
// verbatim in between.
func (s *scanner) value() (string, bool) {
	var buf []byte
	keep := 0 // len(buf) without trailing unquoted whitespace
	quoted := false
	for {
		c, ok := s.next()
		if !ok {
			return string(buf[:keep]), !quoted
		}
		if c == '\n' {
			if quoted {
				return "", false
			}
			s.line++
			return string(buf[:keep]), true
		}
		if !quoted && (c == '#' || c == ';') {
			s.skipLine()
			return string(buf[:keep]), true
		}
		if !quoted && isSpace(c) {
			if len(buf) > 0 {
				buf = append(buf, c)
			}
			continue
		}
		switch c {
		case '\\':
			c, ok = s.next()
			switch {
			case !ok:
				// git treats a backslash at end of file as a line
				// continuation, which shows in its line numbers.
				s.line++
				continue
			case c == '\n':
				s.line++
				continue
			case c == 't':
				c = '\t'
			case c == 'b':
				c = '\b'
			case c == 'n':
				c = '\n'
			case c == '\\' || c == '"':
			default:
				return "", false
			}
		case '"':
			quoted = !quoted
			keep = len(buf)
			continue
		}
		buf = append(buf, c)
		keep = len(buf)
	}
}

// isSpace is git's own isspace: space, tab, LF and CR only (not VT or FF).
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isAlpha(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

func isAlnum(c byte) bool { return isAlpha(c) || '0' <= c && c <= '9' }
