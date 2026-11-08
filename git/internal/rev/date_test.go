package rev

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

var dateInputs = []string{
	"now", "yesterday", "today", "noon", "midnight", "tea", "last friday", "friday",
	"friday noon", "2 days ago", "2.days.ago", "1 month 2 weeks 3 days 1 hour 1 second ago",
	"1979-02-26 18:30:00", "2005-04-07T22:13:13", "2005-04-07T22:13:13.019",
	"2005-04-07T22:13:13Z", "2005-04-07 22:13:13 +0200", "2005.04.07", "04/07/2005",
	"07.04.2005", "Thu, 07 Apr 2005 22:13:13 +0200", "@1700000000", "1700000000",
	"1700000000 +0100", "12345", "garbage", "3 weeks", "last week", "last month", "10am",
	"10pm yesterday", "yesterday 9:14pm", "Apr 7", "April 7 2005", "7 Apr", "never",
	"1 year ago", "5 minutes ago", "2005-04-07", "2005-04", "20050407", "22:13",
	"noon yesterday", "3pm", "12am", "12pm", "midnight yesterday", "Dec 25", "tea yesterday",
	"a week ago", "saturday", "last saturday", "last sunday", "last monday", "2 fridays ago",
	"friday 3pm", "last friday 3pm", "last year", "2 years ago", "Jan 1", "Nov 30",
	"13/07/2005", "07/13/2005", "2005/04/07", "7.4.2005", "2005-04-07 10:00 UTC",
	"2005-04-07 10:00 -0700", "2005-04-07 10:00:00 +05:30", "2005-04-07T10:00:00+05:30",
	"2005-04-07 10:00 PM", "2005-04-07 10pm", "10:00pm", "3 days ago 10am", "10am 3 days ago",
	"1.week.ago", "1 sec ago", "30 mins ago", "5 seconds", "Mon, 7 Apr 2005 22:13:13 -0000",
	"7 Apr 2005", "Apr 2005", "2005", "1999", "1969", "100000000", "99999999", "@99999999",
	"4294967296", "00:00", "23:59:59", "24:00", "today noon", "noon today", "5 min ago",
	"5 hour", "1 days", "2005-04-07 10:00", "10:00 2005-04-07", "yesterday 10:00 +0200",
	"15", "0", "31", "32", "1970", "2099", "2100", "3 5", "Oct", "Dec", "Oct 2005", "5 Oct",
	"last oct", "2 3 days ago", "1 day 1 day ago", "9:14:30", "9:14:30.5", "9 pm", "13pm",
	"0am", "9:14 pm", "12:30am", "noon 3 days ago", "midnight 2 days ago", "tea today",
	"06/07/2005", "07.13.2005", "2005.13.07", "07-04-2005", "7/4", "07.04", "2005-4-7",
	"3 days ago noon", "@0 +0000", "@99999999 +0000", "99999999 +0000", "2 months",
	"Dec 2026", "3 5 7", "12 13", "13 12", "2005 3", "2005-02-30 10:00",
	"Thu Apr 7 22:13:13 2005", "Thu Apr 7 22:13:13 2005 -0700", "22:13:13 2005-04-07 +0100",
	"yesterday at 5pm", "last tuesday at noon", "1.5 days ago", "2005-04-07 noon", "10.10.10",
	"2005-04-07 10:00 EST", "2005-04-07 10:00 PST", "2005-04-07 10:00 CEST", "one week ago",
	"ten days ago", "eleven days ago", "3days ago", "3 Days Ago", "YESTERDAY", "Yest",
	"75", "69", "70", "100", "week ago", "12/25", "13/25", "5 6 7 8", "last 3 days",
	"last hour", "3 last friday", "2005-04-07T10:00:00Z", "10.30", "10:30.5", "Apr 7, 2005",
	"Apr-7-2005", "Thursday, April 7, 2005 10:00", "12 noon", "2am yesterday",
	"last friday noon", "last fri", "last fr", "11/9", "11/18", "11/19", "12/1", "19.11",
	"40", "2040", "11/19/2026", "12/25/2026", "1/1/2030", "36", "38", "12 38", "1/1/37",
	"1/1/38", "1/1/70", "1/1/00", "05",
}

// TestParseDateMatchesGit compares ParseDate with the timestamp git prints for
// `git rev-parse --since=<date>`, in zones with and without DST. Git reads the
// clock itself, so the result must lie between ParseDate at the instants just
// before and after git ran.
func TestParseDateMatchesGit(t *testing.T) {
	g := newGit(t)
	dir := t.TempDir()
	g.run(dir, "init", "-q")
	// zones maps the TZ git gets to the equivalent location ParseDate gets.
	zones := [][2]string{
		{"America/New_York", "America/New_York"},
		{"UTC", "UTC"},
		{"Asia/Kolkata", "Asia/Kolkata"},
		{"Australia/Sydney", "Australia/Sydney"},
	}
	if runtime.GOOS == "windows" {
		// The Windows C runtime reads TZ only as a 3-letter name, an offset
		// and an optional 3-letter DST name that always follows US rules.
		zones = [][2]string{
			{"EST5EDT", "America/New_York"},
			{"UTC", "UTC"},
			{"IST-5:30", "Asia/Kolkata"},
			{"AES-10", "Etc/GMT-10"},
		}
	}
	for _, z := range zones {
		zone := z[0]
		loc, err := time.LoadLocation(z[1])
		if err != nil {
			t.Logf("skipping zone %s: %v", z[1], err)
			continue
		}
		var args []string
		for _, in := range dateInputs {
			args = append(args, "--since="+in)
		}
		before := time.Now().Truncate(time.Second)
		out := g.runEnv(dir, []string{"TZ=" + zone}, append([]string{"rev-parse"}, args...)...)
		after := time.Now()
		lines := strings.Split(out, "\n")
		if len(lines) != len(dateInputs) {
			t.Fatalf("got %d lines for %d inputs", len(lines), len(dateInputs))
		}
		for i, in := range dateInputs {
			// git prints a time before the epoch (1/1/70 east of UTC) as its
			// unsigned 64-bit wraparound.
			u, err := strconv.ParseUint(strings.TrimPrefix(lines[i], "--max-age="), 10, 64)
			if err != nil {
				t.Fatalf("%q: unexpected git output %q", in, lines[i])
			}
			want := int64(u)
			lo := ParseDate(in, before.In(loc)).Unix()
			hi := ParseDate(in, after.In(loc)).Unix()
			// Windows' mktime fails (-1) before the epoch, so git cannot be
			// the oracle there.
			if runtime.GOOS == "windows" && want == -1 && max(lo, hi) < 0 {
				continue
			}
			if want < min(lo, hi) || want > max(lo, hi) {
				t.Errorf("%s: ParseDate(%q) = %d..%d, git %d (diff %+d s)", zone, in, lo, hi, want, lo-want)
			}
		}
	}
}
