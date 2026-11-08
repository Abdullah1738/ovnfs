package merge

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var hunkHeader = regexp.MustCompile(`(?m)^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// gitDiff returns git's hunks for a → b from diff --no-index -U0, which uses
// the same line diff as merge-file once the indent heuristic is off.
func gitDiff(t *testing.T, env []string, dir string, a, b []byte) []hunk {
	t.Helper()
	pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(pa, a, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pb, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "diff", "--no-index", "--no-indent-heuristic", "--no-color", "-U0", "--", pa, pb)
	cmd.Env = env
	out, _ := cmd.Output() // exit status 1 just means the files differ
	var hs []hunk
	num := func(s string) int {
		if s == "" {
			return 1
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	for _, m := range hunkHeader.FindAllStringSubmatch(string(out), -1) {
		h := hunk{A: num(m[1]), NA: num(m[2]), B: num(m[3]), NB: num(m[4])}
		// git numbers an empty side by the line before it.
		if h.NA > 0 {
			h.A--
		}
		if h.NB > 0 {
			h.B--
		}
		hs = append(hs, h)
	}
	return hs
}

type diffCase struct {
	name string
	a, b string
}

// compareDiffWithGit checks diffLines against git for every case, in parallel.
func compareDiffWithGit(t *testing.T, cases []diffCase) {
	needGit(t)
	env := gitEnv(t)
	work := make(chan diffCase)
	var mu sync.Mutex
	failed := 0
	var wg sync.WaitGroup
	for range 8 {
		dir := t.TempDir()
		wg.Go(func() {
			for c := range work {
				want := gitDiff(t, env, dir, []byte(c.a), []byte(c.b))
				var tab internTable
				got := diffLines(tab.ids(tab.lines([]byte(c.a))), tab.ids(tab.lines([]byte(c.b))))
				if fmt.Sprint(got) == fmt.Sprint(want) {
					continue
				}
				mu.Lock()
				if failed++; failed <= 5 {
					t.Errorf("%s:\na %q\nb %q\n got %v\nwant %v", c.name, trim(c.a), trim(c.b), got, want)
				}
				mu.Unlock()
			}
		})
	}
	for _, c := range cases {
		work <- c
	}
	close(work)
	wg.Wait()
	if failed > 0 {
		t.Errorf("%d of %d diffs differ from git", failed, len(cases))
	}
}

func trim(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

func lines(words ...string) string {
	var sb strings.Builder
	for _, w := range words {
		sb.WriteString(w)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestDiffMatchesGitRandom covers alignment tie-breaking and the handling of
// lines that occur often or not at all in the other file.
func TestDiffMatchesGitRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	var cases []diffCase
	for i := range 3000 {
		maxLen, alpha := 31, 2+rng.IntN(5)
		if i%2 == 1 {
			maxLen, alpha = 81, 2+rng.IntN(14)
		}
		gen := func() string {
			ws := make([]string, rng.IntN(maxLen))
			for k := range ws {
				ws[k] = string(rune('a' + rng.IntN(alpha)))
			}
			return lines(ws...)
		}
		cases = append(cases, diffCase{fmt.Sprintf("random %d", i), gen(), gen()})
	}
	compareDiffWithGit(t, cases)
}

// TestDiffMatchesGitLarge covers the limits that only bigger inputs reach:
// the reach of the scan deciding whether a frequent line is noise, and the
// edit cost after which the search settles for a good split.
func TestDiffMatchesGitLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("large inputs")
	}
	var cases []diffCase
	// A block of frequent lines between runs of lines the other file lacks.
	for _, c := range []int{20, 40, 60} {
		for _, side := range []int{60, 90, 110, 150} {
			var a []string
			for i := range side {
				a = append(a, fmt.Sprintf("u%d", i))
			}
			for range c {
				a = append(a, "X")
			}
			for i := range side + 20 {
				a = append(a, fmt.Sprintf("v%d", i))
			}
			b := []string{"p"}
			for range 300 {
				b = append(b, "X")
			}
			cases = append(cases, diffCase{fmt.Sprintf("noise block %d/%d", c, side), lines(a...), lines(append(b, "q")...)})
		}
	}
	rng := rand.New(rand.NewPCG(9, 9))
	random := func(n, alpha int) []string {
		ws := make([]string, n)
		for i := range ws {
			ws[i] = strconv.Itoa(rng.IntN(alpha))
		}
		return ws
	}
	// Unrelated files over a tiny alphabet: the edit cost is huge.
	for _, n := range []int{300, 3000, 20000} {
		cases = append(cases, diffCase{fmt.Sprintf("unrelated %d", n), lines(random(n, 4)...), lines(random(n+n/10, 4)...)})
	}
	// Shuffled blocks: long common runs at a high edit cost.
	base := random(20000, 100)
	var blocks [][]string
	for i := 0; i < len(base); i += 50 {
		blocks = append(blocks, base[i:i+50])
	}
	rng.Shuffle(len(blocks), func(i, j int) { blocks[i], blocks[j] = blocks[j], blocks[i] })
	var shuffled []string
	for _, b := range blocks {
		shuffled = append(shuffled, b...)
	}
	cases = append(cases, diffCase{"shuffled blocks", lines(base...), lines(shuffled...)})
	compareDiffWithGit(t, cases)
}
