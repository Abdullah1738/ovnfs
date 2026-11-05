package grep

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// accelFrags are the pieces randomLines builds lines from: ASCII words and
// runs, multibyte and case-folding runes, digits, and invalid UTF-8.
var accelFrags = strings.Split("a|b|x|f|o|foo| |  |\t|-|_|0|7|2024|-12-|31|é|É|σ|Σ|ς|α|Ωμέγα|٣|"+
	"\u212A|ſ|k|s|\xff|\xc3|\xe2\x82|\xf0\x9f\x98|\U0001F600|日本|impl|(|.|xx|aaaa|"+
	"abababab|\r|a b|x\t|1999-01-02|\x7f|z|K|foo_bar", "|")

// randomLines returns random lines mixing accelFrags with long single-byte
// runs (several vector blocks) and long lines, sometimes without a final
// '\n'.
func randomLines(rng *rand.Rand) []byte {
	const runBytes = " az0x-\t."
	var b []byte
	lines := 1 + rng.IntN(12)
	for li := range lines {
		n := rng.IntN(14)
		if rng.IntN(6) == 0 {
			n = 50 + rng.IntN(250)
		}
		for range n {
			if rng.IntN(10) == 0 {
				b = append(b, strings.Repeat(string(runBytes[rng.IntN(len(runBytes))]), 1+rng.IntN(120))...)
			} else {
				b = append(b, accelFrags[rng.IntN(len(accelFrags))]...)
			}
		}
		if li+1 < lines || rng.IntN(2) == 0 {
			b = append(b, '\n')
		}
	}
	return b
}

// TestAccelerationAgrees checks the DFA, accelerated states included,
// against Go's regexp line by line: next must report exactly the matching
// lines in order, and matches must agree on each line.
func TestAccelerationAgrees(t *testing.T) {
	patterns := []string{
		`\p{Greek}`, `\d{4}-\d{2}-\d{2}`, `\bfoo\b`, `^\s*$`, `[^a]x`, `é+`, `(?i)σ`, `x$`,
		`(?i)k`, `a[ab]{12}b$`, `\w+é`, `[α-ω]{2,}`, `^a`, `o\B`, `[^\x00-\x7f]`, `.x`,
		`\S+\s*$`, `ab*c?$`, `\bx`, `[^ ]{30}`, `(?i)foo_bar`, `\s{3}`, `^$`, `a.*z$`, `[a-z]+\d`,
	}
	rng := rand.New(rand.NewPCG(0x5eed, 1))
	for _, pat := range patterns {
		c, err := compilePattern(pat, false, false, false)
		if err != nil {
			t.Fatal(err)
		}
		re := referenceRegexp(t, pat, false)
		d := newDFA(c.prog)
		for range 40 {
			buf := randomLines(rng)
			from := 0
			for ls := 0; ls < len(buf); {
				le := lineEnd(buf, ls)
				want := re.Match(buf[ls:le])
				if got := d.matches(t.Context(), buf[ls:le]); got != want {
					t.Fatalf("%s: matches(%q) = %v, regexp %v", pat, buf[ls:le], got, want)
				}
				if want {
					if gls, gle, ok := d.next(t.Context(), buf, from); !ok || gls != ls || gle != le {
						t.Fatalf("%s: next(%q, %d) = [%d,%d) %v, want [%d,%d)", pat, buf, from, gls, gle, ok, ls, le)
					}
					from = le + 1
				}
				ls = le + 1
			}
			if from < len(buf) {
				if gls, gle, ok := d.next(t.Context(), buf, from); ok {
					t.Fatalf("%s: next(%q, %d) = [%d,%d), want no match", pat, buf, from, gls, gle)
				}
			}
		}
	}
}

// Start states that loop on most bytes must be accelerated, or the
// agreement test above never exercises a skip.
func TestAccelerationEngages(t *testing.T) {
	const text = "plain ascii text without any hit at all, long enough for several vector blocks\nnext line\n"
	for _, tc := range []struct {
		pattern string
		want    accel
	}{
		{`\p{Greek}`, accelYes},
		{`\d{4}-\d{2}-\d{2}`, accelYes},
		{`x$`, accelYes},
		{`(?i)σ`, accelYes},
		{`\bfoo\b`, accelNo},    // a word byte leaves the after-non-word state
		{`^\s*$`, accelUnknown}, // the start state records the line start: no byte loops to it
	} {
		c, err := compilePattern(tc.pattern, false, false, false)
		if err != nil {
			t.Fatal(err)
		}
		d := newDFA(c.prog)
		if _, _, ok := d.next(t.Context(), []byte(text), 0); ok {
			t.Fatalf("%s: matched %q", tc.pattern, text)
		}
		if st := d.states[0]; st.accel != tc.want || tc.want == accelYes && !st.nlLoop {
			t.Errorf("%s: start state accel=%d nlLoop=%v, want accel=%d crossing lines", tc.pattern, st.accel, st.nlLoop, tc.want)
		}
	}
}
