package grep

import (
	"context"
	"unicode/utf8"
)

// This file renders matching lines wider than MaxColumns as pi-walker's
// grep/render.rs does: the kept window is centred on the line's first match
// and the match's character column is reported, while context lines keep
// the plain head cut of scanner.line.

// matched renders the matching block buf[bs:be] (be: the '\n' ending its
// last line, or len(buf)) like [scanner.line] and, when it is wider than
// maxColumns, windows it around its first match. It returns the text,
// whether it was cut, and the match's 1-based character column (0 when not
// cut or no match was located).
func (sc *scanner) matched(ctx context.Context, buf []byte, bs, be, maxColumns int) (string, bool, int) {
	b := trimSpaceRight(ctx, buf[bs:be])
	if ctx.Err() != nil {
		return "", false, 0
	}
	sc.reserve(3*len(b) + 3)
	start := len(sc.text)
	lossy := !utf8.Valid(b)
	if lossy {
		sc.text = appendLossy(ctx, sc.text, b)
	} else {
		sc.text = append(sc.text, b...)
	}
	if maxColumns <= 0 || len(sc.text)-start <= maxColumns {
		return sc.str(start), false, 0
	}
	line := sc.text[start:]
	ms, me, found := sc.firstMatch(buf, bs, be, line, lossy)
	from, to, lead, trail, column := windowLine(line, maxColumns, ms, me, found)
	// The window is narrower than line, so it is rebuilt in place (from is
	// 0 unless lead).
	n := to - from
	if lead {
		copy(sc.text[start+3:], line[from:to])
		copy(sc.text[start:], "...")
		n += 3
	}
	sc.text = sc.text[:start+n]
	if trail {
		sc.text = append(sc.text, "..."...)
	}
	return sc.str(start), true, column
}

// firstMatch locates the first match of the block buf[bs:be] in line, the
// block rendered (trimmed and, when lossy, decoded with replacements), as
// render.rs's first_match_in_line: the pattern is re-run from the block's
// start (bs is a line start, so the look-behind of '^' and '\b' is the
// same), a line-mode block without its '\n', a multiline block with the
// rest of buf for look-ahead.
func (sc *scanner) firstMatch(buf []byte, bs, be int, line []byte, lossy bool) (start, end int, ok bool) {
	var loc []int
	if sc.c.locate != nil {
		if re := sc.c.locate(); re != nil {
			limit := be
			if sc.c.multiline {
				limit = len(buf)
			}
			loc = re.FindIndex(buf[bs:limit])
		}
	}
	if loc == nil {
		return 0, 0, false
	}
	// The block with its terminator, relative to bs.
	terminated := be < len(buf)
	blockEnd := be - bs
	if terminated {
		blockEnd++
	}
	// A match at the block's end begins on the next line, except an empty
	// one ('$', '\z') after an unterminated last line.
	if loc[0] > blockEnd || (loc[0] == blockEnd && terminated) {
		return 0, 0, false
	}
	block := buf[bs : bs+blockEnd]
	// Offsets carry over unless lossy decoding replaced invalid bytes; one
	// in the trimmed trailing whitespace maps to the end of line.
	toLine := func(off int) int {
		if lossy {
			off = lossyLen(block[:off])
		}
		return floorBoundary(line, min(off, len(line)))
	}
	return toLine(loc[0]), toLine(min(loc[1], blockEnd)), true
}

// windowLine fits line, wider than maxColumns bytes, into maxColumns bytes
// so its first match (line[start:end] when found) stays visible, as
// render.rs's window_matched_line: when the match ends inside the head the
// plain cut keeps, the line is cut the same way (line[:to] plus "...");
// otherwise the kept window line[from:to] is centred on the match and each
// cut side (lead, trail) is marked with "...". column is the match's 1-based
// character column, 0 without a match.
func windowLine(line []byte, maxColumns, start, end int, found bool) (from, to int, lead, trail bool, column int) {
	headEnd := floorBoundary(line, max(0, maxColumns-3))
	if !found {
		return 0, headEnd, false, true, 0
	}
	start = floorBoundary(line, start)
	column = utf8.RuneCount(line[:start]) + 1
	body := max(0, maxColumns-6)
	if end <= headEnd || body == 0 {
		return 0, headEnd, false, true, column
	}
	// Centre the match; a match wider than the window shows from its start.
	from = max(0, start-max(0, body-(end-start))/2)
	if from+body >= len(line) {
		// The window reaches the end of the line: only its start is cut.
		return ceilBoundary(line, len(line)-max(0, maxColumns-3)), len(line), true, false, column
	}
	from = ceilBoundary(line, from)
	to = floorBoundary(line, from+body)
	return from, to, from != 0, true, column
}

// floorBoundary is the largest character boundary of the valid UTF-8 s at
// or before i (len(s) past the end).
func floorBoundary(s []byte, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// ceilBoundary is the smallest character boundary of the valid UTF-8 s at
// or after i (len(s) past the end).
func ceilBoundary(s []byte, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return min(i, len(s))
}

// lossyLen is len(appendLossy(nil, b)): the length of b with each maximal
// invalid UTF-8 subpart replaced by U+FFFD.
func lossyLen(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, w := utf8.DecodeRune(b)
		if r == utf8.RuneError && w == 1 {
			w = invalidPrefix(b)
			n += utf8.RuneLen(utf8.RuneError)
		} else {
			n += w
		}
		b = b[w:]
	}
	return n
}
