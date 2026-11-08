package rev

import (
	"strconv"
	"strings"
	"time"
)

// ParseDate interprets s as git does for <rev>@{<date>}: first as an exact
// date (ISO 8601, RFC 2822, "@<unix> <zone>", a raw timestamp), else as an
// approximate one ("yesterday", "3 days ago", "last friday noon", "Apr 7"),
// relative to now in now's location. Like git it never fails: words it does
// not understand are ignored, so pure garbage yields now.
//
// The quirks below were all established by comparing with
// `git rev-parse --since=<date>`, which prints the parsed timestamp.
func ParseDate(s string, now time.Time) time.Time {
	toks := tokenizeDate(s)
	if t, ok := parseExactDate(toks, now); ok {
		return t
	}
	return parseApproxDate(toks, now)
}

type dateTokKind uint8

const (
	tokWord   dateTokKind = iota + 1
	tokNumber             // one or more digit runs joined by - / . :
	tokZone               // +hhmm, -hh:mm
	tokAt                 // '@' directly before a number
	tokComma
)

type dateTok struct {
	kind  dateTokKind
	word  string   // tokWord: lower-cased
	nums  []int    // tokNumber parts
	lens  []int    // digit count of each part
	seps  []byte   // tokNumber separators between parts
	zone  int      // tokZone: offset in seconds
	tail  *dateTok // fractional digits after a time ("9:14:30.5")
	valid bool     // tokZone parsed
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func tokenizeDate(s string) []dateTok {
	var toks []dateTok
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isAlpha(c):
			j := i
			for j < len(s) && isAlpha(s[j]) {
				j++
			}
			toks = append(toks, dateTok{kind: tokWord, word: strings.ToLower(s[i:j])})
			i = j
		case isDigit(c):
			t, j := scanNumber(s, i)
			toks = append(toks, t)
			i = j
		case (c == '+' || c == '-') && i+1 < len(s) && isDigit(s[i+1]) && (i == 0 || !isAlpha(s[i-1])):
			// A zone: hh, hhmm or hh:mm. Anything else is noise. After a
			// word ("Apr-7-2005") the sign is just a separator.
			j := i + 1
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			digits := s[i+1 : j]
			if j+2 < len(s) && s[j] == ':' && len(digits) == 2 && isDigit(s[j+1]) && isDigit(s[j+2]) {
				digits += s[j+1 : j+3]
				j += 3
			}
			t := dateTok{kind: tokZone}
			if n, err := strconv.Atoi(digits); err == nil && (len(digits) == 4 || len(digits) == 2) {
				if len(digits) == 2 {
					n *= 100
				}
				if n%100 < 60 && n/100 <= 24 {
					t.zone = (n/100*60 + n%100) * 60
					if c == '-' {
						t.zone = -t.zone
					}
					t.valid = true
				}
			}
			toks = append(toks, t)
			i = j
		case c == '@' && i+1 < len(s) && isDigit(s[i+1]):
			toks = append(toks, dateTok{kind: tokAt})
			i++
		case c == ',':
			toks = append(toks, dateTok{kind: tokComma})
			i++
		default:
			i++
		}
	}
	return toks
}

// scanNumber reads digit runs joined by single - / . : separators. A time
// (one with ':') ends at its seconds; a following ".digits" becomes a
// separate fraction token, which exact dates ignore and approximate dates
// read as a day of the month (git does).
func scanNumber(s string, i int) (dateTok, int) {
	t := dateTok{kind: tokNumber}
	for {
		j := i
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		n, err := strconv.Atoi(s[i:j])
		if err != nil {
			n = -1 // overflow: never a valid date part
		}
		t.nums = append(t.nums, n)
		t.lens = append(t.lens, j-i)
		i = j
		if i+1 >= len(s) || !strings.ContainsRune("-/.:", rune(s[i])) || !isDigit(s[i+1]) {
			return t, i
		}
		sep := s[i]
		if sep == '.' && strings.IndexByte(string(t.seps), ':') >= 0 {
			frac, k := scanNumber(s, i+1)
			t.tail = &frac
			return t, k
		}
		t.seps = append(t.seps, sep)
		i++
	}
}

var monthNames = [...]string{"january", "february", "march", "april", "may", "june",
	"july", "august", "september", "october", "november", "december"}

var weekdayNames = [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

var numberWords = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10}

// zoneNames are the zone abbreviations git accepts, in seconds east of UTC.
var zoneNames = map[string]int{
	"utc": 0, "ut": 0, "gmt": 0, "z": 0,
	"est": -5 * 3600, "edt": -4 * 3600, "cst": -6 * 3600, "cdt": -5 * 3600,
	"mst": -7 * 3600, "mdt": -6 * 3600, "pst": -8 * 3600, "pdt": -7 * 3600,
	"cet": 3600, "cest": 2 * 3600, "met": 3600, "mest": 2 * 3600,
	"eet": 2 * 3600, "eest": 3 * 3600, "bst": 3600, "wet": 0, "west": 3600,
	"jst": 9 * 3600, "ist": 19800,
}

// unitSeconds maps fixed-length units to seconds; days and longer move the
// calendar instead so that month lengths and DST are honoured.
var unitSeconds = map[string]int64{"second": 1, "minute": 60, "hour": 3600}

// prefixOf reports whether w abbreviates name to at least three letters.
func prefixOf(w, name string) bool { return len(w) >= 3 && strings.HasPrefix(name, w) }

func monthIndex(w string) int {
	for i, m := range monthNames {
		if prefixOf(w, m) {
			return i + 1
		}
	}
	return 0
}

func weekdayIndex(w string) int {
	for i, d := range weekdayNames {
		if prefixOf(w, d) || w == d+"s" {
			return i
		}
	}
	return -1
}

// unitOf maps "day"/"days" and friends to their singular name.
func unitOf(w string) string {
	w = strings.TrimSuffix(w, "s")
	switch w {
	case "second", "minute", "hour", "day", "week", "month", "year":
		return w
	}
	return ""
}

// fields is a partially specified local date and time; -1 means unset.
type fields struct {
	year, month, day, hour, min, sec int
	zone                             int
	zoneSet                          bool
	timestamp                        int64 // >= 0 once a raw timestamp was seen
}

func newFields() fields {
	return fields{year: -1, month: -1, day: -1, hour: -1, min: -1, sec: -1, timestamp: -1}
}

// twoDigitYear maps "yy" as git does: 00-37 is 20yy, 70-99 is 19yy and the
// rest is no year at all.
func twoDigitYear(n int) int {
	switch {
	case n >= 0 && n < 38:
		return 2000 + n
	case n >= 70 && n < 100:
		return 1900 + n
	}
	return -1
}

func validYear4(n int) bool { return n >= 1970 && n <= 2099 }

// yearOf interprets a numeric year part by its digit count.
func yearOf(n, digits int) int {
	switch digits {
	case 4:
		if validYear4(n) {
			return n
		}
	case 1, 2:
		return twoDigitYear(n)
	}
	return -1
}

// validDay checks ranges only: git lets "02-30" roll over into March.
func validDay(month, day int) bool {
	return month >= 1 && month <= 12 && day >= 1 && day <= 31
}

// setTime applies "H:M[:S]".
func (f *fields) setTime(t dateTok) bool {
	if len(t.nums) > 3 || slicesContainsNon(t.seps, ':') {
		return false
	}
	h, m, s := t.nums[0], t.nums[1], 0
	if len(t.nums) == 3 {
		s = t.nums[2]
	}
	if h < 0 || h > 24 || m < 0 || m > 59 || s < 0 || s > 60 {
		return false
	}
	f.hour, f.min, f.sec = h, m, s
	return true
}

func slicesContainsNon(seps []byte, want byte) bool {
	for _, c := range seps {
		if c != want {
			return true
		}
	}
	return false
}

// tooFarAhead reports a slash-style date lying more than ten days in the
// future within the current year, which git rejects as a misreading (it then
// tries the other order, then the parts as loose numbers). Observed: git
// takes the date at now's local wall-clock time as if it were UTC, so zones
// east of UTC reject the tenth day already.
func tooFarAhead(year, month, day int, now time.Time) bool {
	if year != now.Year() {
		return false
	}
	h, m, s := now.Clock()
	d := time.Date(year, time.Month(month), day, h, m, s, 0, time.UTC)
	return d.After(now.Add(10 * 24 * time.Hour))
}

// setDate applies a numeric date; it reports false when no reading fits,
// leaving the caller to treat the parts as loose numbers.
func (f *fields) setDate(t dateTok, now time.Time) bool {
	n, l := t.nums, t.lens
	if l[0] == 4 {
		if !validYear4(n[0]) {
			return false
		}
		if len(n) == 2 {
			// "2005-04" is a year and then a loose number (git reads it so).
			f.year = n[0]
			f.number(n[1], l[1], now)
			return true
		}
		m, d := n[1], n[2]
		if !validDay(m, d) {
			m, d = d, m
			if !validDay(m, d) {
				return false
			}
		}
		f.year, f.month, f.day = n[0], m, d
		return true
	}
	year := -1
	checkYear := now.Year()
	if len(n) == 3 {
		year = yearOf(n[2], l[2])
		if year >= 0 {
			checkYear = year
		}
	}
	dotted := t.seps[0] == '.'
	try := func(m, d int) bool {
		if !validDay(m, d) {
			return false
		}
		if !dotted && tooFarAhead(checkYear, m, d, now) {
			return false
		}
		f.month, f.day = m, d
		if year >= 0 {
			f.year = year
		}
		return true
	}
	if dotted {
		return try(n[1], n[0]) || try(n[0], n[1])
	}
	return try(n[0], n[1]) || try(n[1], n[0])
}

// number places a loose number: a raw timestamp, a four-digit year, else the
// first free of day, month and two-digit year.
func (f *fields) number(n, digits int, now time.Time) {
	switch {
	case n >= 100000000 && n < 1<<32:
		f.timestamp = int64(n)
	case digits == 4:
		if validYear4(n) && f.year < 0 {
			f.year = n
		}
	case digits > 2 || n <= 0:
	case f.day < 0 && n <= 31:
		f.day = n
	case f.month < 0 && n <= 12:
		f.month = n
	case f.year < 0:
		f.year = twoDigitYear(n)
	}
}

// parseExactDate accepts only fully specified dates (year, month, day, hour
// and minute, optional zone) and raw timestamps, every token understood. A
// date without a zone is local time at that date, DST included.
func parseExactDate(toks []dateTok, now time.Time) (time.Time, bool) {
	f := newFields()
	pm, am := false, false
	for i, t := range toks {
		switch t.kind {
		case tokComma:
		case tokAt:
			// "@<unix>" is a raw timestamp only with a zone after it.
			if i+2 < len(toks) && toks[i+1].kind == tokNumber && len(toks[i+1].nums) == 1 &&
				toks[i+2].kind == tokZone && toks[i+2].valid && i+3 == len(toks) {
				return time.Unix(int64(toks[i+1].nums[0]), 0), true
			}
			return time.Time{}, false
		case tokZone:
			if !t.valid || f.zoneSet {
				return time.Time{}, false
			}
			f.zone, f.zoneSet = t.zone, true
		case tokWord:
			switch {
			case monthIndex(t.word) > 0 && f.month < 0:
				f.month = monthIndex(t.word)
			case weekdayIndex(t.word) >= 0:
			case t.word == "t" && f.day >= 0:
			case t.word == "pm":
				pm = true
			case t.word == "am":
				am = true
			default:
				z, ok := zoneNames[t.word]
				if !ok || f.zoneSet {
					return time.Time{}, false
				}
				f.zone, f.zoneSet = z, true
			}
		case tokNumber:
			switch {
			case len(t.seps) > 0 && t.seps[0] == ':':
				if f.hour >= 0 || !f.setTime(t) {
					return time.Time{}, false
				}
			case len(t.nums) > 1:
				if f.day >= 0 || !f.setDate(t, now) {
					return time.Time{}, false
				}
			default:
				f.number(t.nums[0], t.lens[0], now)
			}
		}
	}
	if f.timestamp >= 0 {
		return time.Unix(f.timestamp, 0), true
	}
	if f.year < 0 || f.month < 0 || f.day < 0 || f.hour < 0 {
		return time.Time{}, false
	}
	if pm && f.hour < 12 {
		f.hour += 12
	} else if am && f.hour == 12 {
		f.hour = 0
	}
	loc := now.Location()
	if f.zoneSet {
		loc = time.FixedZone("", f.zone)
	}
	return time.Date(f.year, time.Month(f.month), f.day, f.hour, f.min, f.sec, 0, loc), true
}

// approx is the state of an approximate date being read.
type approx struct {
	f            fields
	now          time.Time
	pending      int // number waiting for a unit, -1 if none
	pendingLen   int
	days, months int
	years        int
	seconds      int64
	dateSet      bool // the day was named, explicitly or relatively
	yesterday    bool
	today        bool
	backIfFuture bool // "noon"/"tea" without a day mean the last one passed
	monthNamed   bool // month set by name or loose number: future means last year
}

func parseApproxDate(toks []dateTok, now time.Time) time.Time {
	a := approx{f: newFields(), now: now, pending: -1}
	for _, t := range toks {
		switch t.kind {
		case tokWord:
			if t.word == "never" {
				return time.Unix(0, 0)
			}
			a.word(t.word)
		case tokNumber:
			a.flush()
			a.numberTok(t)
		case tokAt, tokZone, tokComma:
			// Approximate dates ignore zones; "@" alone is noise.
		}
		if a.f.timestamp >= 0 {
			return time.Unix(a.f.timestamp, 0)
		}
	}
	a.flush()
	if a.f.timestamp >= 0 {
		return time.Unix(a.f.timestamp, 0)
	}
	return a.result()
}

// flush places a number no unit claimed.
func (a *approx) flush() {
	if a.pending < 0 {
		return
	}
	n, l := a.pending, a.pendingLen
	a.pending = -1
	month := a.f.month
	a.f.number(n, l, a.now)
	if a.f.month != month {
		a.monthNamed = true
	}
}

func (a *approx) numberTok(t dateTok) {
	switch {
	case len(t.nums) == 1:
		a.pending, a.pendingLen = t.nums[0], t.lens[0]
	case t.seps[0] == ':':
		a.f.setTime(t) // an impossible time is ignored
	default:
		if !a.f.setDate(t, a.now) {
			for i, n := range t.nums {
				a.f.number(n, t.lens[i], a.now)
			}
		}
	}
	if t.tail != nil {
		a.numberTok(*t.tail)
		a.flush()
	}
}

func (a *approx) word(w string) {
	if n, ok := numberWords[w]; ok {
		a.flush()
		a.pending, a.pendingLen = n, len(strconv.Itoa(n))
		return
	}
	if u := unitOf(w); u != "" {
		if a.pending < 0 {
			return
		}
		n := a.pending
		a.pending = -1
		switch u {
		case "day":
			a.days += n
		case "week":
			a.days += 7 * n
		case "month":
			a.months += n
		case "year":
			a.years += n
		default:
			a.seconds += int64(n) * unitSeconds[u]
			return
		}
		a.dateSet = true
		return
	}
	if d := weekdayIndex(w); d >= 0 {
		if a.pending < 0 {
			return // a bare weekday names nothing (git ignores it)
		}
		n := a.pending
		a.pending = -1
		back := (int(a.now.Weekday()) - d + 7) % 7
		if back == 0 {
			back = 7
		}
		a.days += back + 7*(n-1)
		a.dateSet = true
		return
	}
	switch w {
	case "last":
		if a.pending < 0 {
			a.pending, a.pendingLen = 1, 1
		}
		return
	case "am", "pm":
		a.ampm(w == "pm")
		return
	case "ago":
		return
	}
	a.flush()
	if m := monthIndex(w); m > 0 {
		a.f.month = m
		a.monthNamed = true
		return
	}
	switch w {
	case "yesterday":
		a.yesterday = true
		a.dateSet = true
	case "today":
		a.today = true
		a.dateSet = true
	case "midnight":
		a.f.hour, a.f.min, a.f.sec = 0, 0, 0
	case "noon":
		a.f.hour, a.f.min, a.f.sec = 12, 0, 0
		a.backIfFuture = true
	case "tea":
		a.f.hour, a.f.min, a.f.sec = 17, 0, 0
		a.backIfFuture = true
	}
}

// ampm applies "am"/"pm" to a just-read number ("9pm") or else to the time
// set earlier ("9:14 pm") or, failing that, to now's time.
func (a *approx) ampm(pm bool) {
	if a.pending >= 0 {
		n := a.pending
		switch {
		case n > 0 && n <= 12:
			a.f.hour = n % 12
			if pm {
				a.f.hour += 12
			}
			a.f.min, a.f.sec = 0, 0
			a.pending = -1
			return
		case n > 12 && n < 24:
			a.f.hour, a.f.min, a.f.sec = n, 0, 0
			a.pending = -1
			return
		}
		a.flush()
	}
	if a.f.hour < 0 {
		a.f.hour, a.f.min, a.f.sec = a.now.Clock()
	}
	switch {
	case pm && a.f.hour < 12:
		a.f.hour += 12
	case !pm && a.f.hour >= 12:
		a.f.hour -= 12
	}
}

func (a *approx) result() time.Time {
	now := a.now
	f := a.f
	year, month, day := now.Date()
	hour, min, sec := now.Clock()
	if f.year >= 0 || f.month >= 0 || f.day >= 0 {
		a.dateSet = true
	}
	if f.month >= 0 {
		if f.year < 0 && a.monthNamed && f.month > int(month) {
			year--
		}
		month = time.Month(f.month)
	}
	if f.year >= 0 {
		year = f.year
	}
	if f.day >= 0 {
		day = f.day
	}
	timeSet := f.hour >= 0
	if timeSet {
		hour, min, sec = f.hour, f.min, f.sec
	} else if a.today {
		hour, min, sec = 0, 0, 0
	}
	if a.yesterday {
		day--
	}
	year -= a.years
	month -= time.Month(a.months)
	day -= a.days
	// Git computes approximate dates with the zone offset in force now, not
	// the one at the resulting date.
	_, offset := now.Zone()
	t := time.Date(year, month, day, hour, min, sec, 0, time.FixedZone("", offset))
	t = t.Add(-time.Duration(a.seconds) * time.Second)
	if a.backIfFuture && !a.dateSet && t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}
