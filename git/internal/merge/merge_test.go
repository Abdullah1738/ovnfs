package merge

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// gitEnv isolates git from the user's and the system's configuration.
func gitEnv(t *testing.T) []string {
	home := t.TempDir()
	return append(os.Environ(),
		"HOME="+home, "XDG_CONFIG_HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "LC_ALL=C")
}

// mergeCase is one three-way merge to compare against git merge-file.
type mergeCase struct {
	name               string
	ours, base, theirs []byte
	opts               Options
}

// gitMergeFile runs git merge-file -p on c and returns its output and exit status.
func gitMergeFile(t *testing.T, env []string, dir string, c mergeCase) ([]byte, int) {
	paths := [3]string{filepath.Join(dir, "ours"), filepath.Join(dir, "base"), filepath.Join(dir, "theirs")}
	for i, data := range [][]byte{c.ours, c.base, c.theirs} {
		if err := os.WriteFile(paths[i], data, 0o644); err != nil {
			t.Error(err)
			return nil, -1
		}
	}
	args := []string{"merge-file", "-p"}
	switch c.opts.Style {
	case StyleDiff3:
		args = append(args, "--diff3")
	case StyleZDiff3:
		args = append(args, "--zdiff3")
	}
	l := c.opts.Labels
	args = append(args, "-L", l.Ours, "-L", l.Base, "-L", l.Theirs, "--", paths[0], paths[1], paths[2])
	cmd := exec.Command("git", args...)
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return out, 0
	case errors.As(err, &exit):
		return out, exit.ExitCode()
	}
	t.Errorf("git merge-file: %v", err)
	return nil, -1
}

// compareWithGit merges every case in-process and with git (in parallel) and
// reports each difference in bytes or conflict count.
func compareWithGit(t *testing.T, cases []mergeCase) {
	needGit(t)
	env := gitEnv(t)
	work := make(chan mergeCase)
	var mu sync.Mutex
	differ, decisions := 0, 0
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		dir := t.TempDir()
		wg.Go(func() {
			for c := range work {
				want, code := gitMergeFile(t, env, dir, c)
				if code < 0 {
					continue
				}
				got, n, err := File(c.ours, c.base, c.theirs, c.opts)
				mu.Lock()
				switch {
				case code >= 128:
					if err == nil {
						t.Errorf("%s: git refused the merge (status %d), File did not", c.name, code)
					}
				case err != nil:
					t.Errorf("%s: File: %v; git merged with status %d", c.name, err, code)
				case !bytes.Equal(got, want) || min(n, 127) != code:
					if (n == 0) != (code == 0) {
						decisions++
					}
					if differ++; differ <= 5 {
						t.Errorf("%s (style %d, labels %q):\nours   %q\nbase   %q\ntheirs %q\n got %d %q\nwant %d %q",
							c.name, c.opts.Style, c.opts.Labels, trim(string(c.ours)), trim(string(c.base)), trim(string(c.theirs)),
							n, trim(string(got)), code, trim(string(want)))
					}
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
	if differ > 0 {
		t.Errorf("%d of %d merges differ from git; %d disagree on clean vs conflicted", differ, len(cases), decisions)
	}
}

var allStyles = []Style{StyleMerge, StyleDiff3, StyleZDiff3}

// everyStyle repeats a merge in every conflict style.
func everyStyle(name, ours, base, theirs string, labels Labels) []mergeCase {
	var cs []mergeCase
	for _, st := range allStyles {
		cs = append(cs, mergeCase{name, []byte(ours), []byte(base), []byte(theirs), Options{labels, st}})
	}
	return cs
}

// randomCases builds seeded random merges: files of up to maxLines lines over
// a tiny alphabet, so that equal lines are frequent, with each side derived
// from base by up to maxEdits random edits (or occasionally unrelated), and
// random CRLF line endings and missing final newlines.
func randomCases(seed uint64, n, maxLines, maxEdits int) []mergeCase {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	labels := []Labels{{"ours", "base", "theirs"}, {}, {"a b", "", "c"}}
	var cases []mergeCase
	for i := range n {
		alpha := 2 + rng.IntN(5)
		word := func() string { return string(rune('a' + rng.IntN(alpha))) }
		words := func(n int) []string {
			ws := make([]string, n)
			for k := range ws {
				ws[k] = word()
			}
			return ws
		}
		base := words(rng.IntN(maxLines + 1))
		side := func() []string {
			if rng.IntN(8) == 0 {
				return words(rng.IntN(maxLines + 1))
			}
			s := append([]string(nil), base...)
			for range rng.IntN(maxEdits + 1) {
				p := rng.IntN(len(s) + 1)
				switch rng.IntN(3) {
				case 0:
					ins := words(1 + rng.IntN(3))
					for k := range ins {
						if rng.IntN(2) == 0 {
							ins[k] = strings.ToUpper(ins[k])
						}
					}
					s = append(s[:p], append(ins, s[p:]...)...)
				case 1:
					if p < len(s) {
						s = append(s[:p], s[min(len(s), p+1+rng.IntN(3)):]...)
					}
				default:
					if p < len(s) {
						s[p] = strings.ToUpper(word())
					}
				}
			}
			return s
		}
		crlf := rng.IntN(4) // 0: LF, 1: CRLF, 2: mixed per line, 3: LF
		render := func(ws []string) string {
			var b strings.Builder
			for _, w := range ws {
				b.WriteString(w)
				if crlf == 1 || crlf == 2 && rng.IntN(3) == 0 {
					b.WriteByte('\r')
				}
				b.WriteByte('\n')
			}
			s := b.String()
			if rng.IntN(4) == 0 {
				s = strings.TrimRight(s, "\r\n")
			}
			return s
		}
		ours, b, theirs := render(side()), render(base), render(side())
		cases = append(cases, everyStyle(fmt.Sprintf("seed %d case %d", seed, i), ours, b, theirs, labels[rng.IntN(len(labels))])...)
	}
	return cases
}

func TestFileMatchesGitRandom(t *testing.T) {
	cases := randomCases(1, 3000, 30, 4)
	if !testing.Short() {
		cases = append(cases, randomCases(2, 1000, 60, 12)...)
		cases = append(cases, randomCases(3, 300, 150, 30)...)
	}
	compareWithGit(t, cases)
}

func TestFileMatchesGitEdgeCases(t *testing.T) {
	type tc struct{ name, ours, base, theirs string }
	long := strings.Repeat("0123456789", 20000)
	tcs := []tc{
		{"clean, both sides", "A\nb\nc\nd\ne\n", "a\nb\nc\nd\ne\n", "a\nb\nc\nd\nE\n"},
		{"unchanged", "a\nb\n", "a\nb\n", "a\nb\n"},
		{"only ours", "a\nB\n", "a\nb\n", "a\nb\n"},
		{"only theirs", "a\nb\n", "a\nb\n", "a\nB\nc\n"},
		{"same change", "a\nX\nc\n", "a\nb\nc\n", "a\nX\nc\n"},
		{"same insertion", "a\nX\nY\nb\n", "a\nb\n", "a\nX\nY\nb\n"},
		{"same deletion", "a\nc\n", "a\nb\nc\n", "a\nc\n"},
		{"overlapping", "a\nB\nc\n", "a\nb\nc\n", "a\nX\nc\n"},
		{"adjacent changes", "A\nb\nc\n", "a\nb\nc\n", "a\nB\nc\n"},
		{"change next to insertion", "A\nb\nc\n", "a\nb\nc\n", "a\nX\nb\nc\n"},
		{"insertion next to change", "a\nX\nb\nc\n", "a\nb\nc\n", "a\nB\nc\n"},
		{"changes one line apart", "A\nb\nc\n", "a\nb\nc\n", "a\nb\nC\n"},
		{"different insertions", "a\nX\nb\n", "a\nb\n", "a\nY\nb\n"},
		{"shared leading and trailing lines", "1\nx\nA\ny\n2\n", "1\na\nb\nc\n2\n", "1\nx\nB\ny\n2\n"},
		{"partly equal insertions", "a\nQ\nR\nc\n", "a\nb\nc\n", "a\nQ\nc\n"},
		{"joined across 3 lines", "A\na\n1\n2\nB\n", "a\na\n1\n2\nb\n", "X\na\n1\n2\nY\n"},
		{"apart across 4 lines", "A\na\n1\n2\n3\nB\n", "a\na\n1\n2\n3\nb\n", "X\na\n1\n2\n3\nY\n"},
		{"joined across punctuation", "A\n}\n}\n\n{\n;\nB\n", "a\n}\n}\n\n{\n;\nb\n", "X\n}\n}\n\n{\n;\nY\n"},
		{"apart across one letter", "A\n.\n.\n.\nz\nB\n", "a\n.\n.\n.\nz\nb\n", "X\n.\n.\n.\nz\nY\n"},
		{"non-ASCII is not a letter", "A\né\né\né\né\nB\n", "a\né\né\né\né\nb\n", "X\né\né\né\né\nY\n"},
		{"same change between conflicts", "A\n.\nQ\n.\nB\n", "a\n.\nq\n.\nb\n", "X\n.\nQ\n.\nY\n"},
		{"one-sided change between conflicts", "A\n1\nQ\n2\nB\n", "a\n1\nq\n2\nb\n", "X\n1\nq\n2\nY\n"},
		{"narrowed conflicts joined", "1\nx\nA\nz\nC\n2\n", "1\na\nb\n2\n", "1\nx\nB\nz\nD\n2\n"},
		{"narrowed conflicts apart", "1\nx\nA\nk\nk\nk\nk\nC\n2\n", "1\na\nb\n2\n", "1\nx\nB\nk\nk\nk\nk\nD\n2\n"},
		{"insertions at start", "X\na\nb\n", "a\nb\n", "Y\na\nb\n"},
		{"insertions at end", "a\nb\nX\n", "a\nb\n", "a\nb\nY\n"},
		{"all empty", "", "", ""},
		{"empty base, same", "a\n", "", "a\n"},
		{"empty base", "A\nb\nc\n", "", "X\nb\nc\n"},
		{"ours emptied", "", "a\nb\n", "a\nB\n"},
		{"both emptied", "", "a\nb\n", ""},
		{"theirs emptied, ours unchanged", "a\nb\n", "a\nb\n", ""},
		{"no newline, conflict at end", "a\nb\nO", "a\nb\nc", "a\nb\nT"},
		{"no newline in ours only", "a\nB", "a\nb\n", "a\nT\n"},
		{"no newline in theirs only", "a\nB\n", "a\nb\n", "a\nT"},
		{"no newline in base only", "a\nB\n", "a\nb", "a\nT\n"},
		{"newline added vs line added", "a\nc\n", "a\nc", "a\nc\nd"},
		{"no newline, clean", "a\nc\nd\n", "a\nc", "a\nc"},
		{"CRLF", "z\r\nb\r\nq\r\n", "z\r\na\r\nq\r\n", "z\r\nc\r\nq\r\n"},
		{"CRLF without final newline", "b", "a\r\n", "c\r\n"},
		{"CRLF sides, LF base", "b\r\n", "a\n", "c\r\n"},
		{"LF before CRLF conflict", "z\nb\r\n", "z\na\r\n", "z\nc\r\n"},
		{"CRLF before LF conflict", "z\r\nb\n", "z\r\na\n", "z\r\nc\n"},
		{"CRLF, empty ours", "", "a\r\n", "c\r\n"},
		{"long lines", long + "A\n" + long + "\n", long + "\n" + long + "\n", long + "B\n" + long + "\n"},
		{"long line conflict", "a\n" + long + "1\n", "a\nb\n", "a\n" + long + "2\n"},
		{"deletion vs modification", "a\nc\n", "a\nb\nc\n", "a\nB\nc\n"},
		{"block deletion vs inner change", "a\ne\n", "a\nb\nc\nd\ne\n", "a\nb\nC\nd\ne\n"},
		{"deletion vs adjacent deletion", "a\nc\nd\n", "a\nb\nc\nd\n", "a\nb\nd\n"},
		{"NUL in ours", "x\x00\n", "a\n", "b\n"},
		{"NUL in base", "a\n", "x\x00\n", "b\n"},
		{"NUL in theirs", "a\n", "b\n", "x\x00\n"},
		{"NUL at byte 7999", strings.Repeat("x", 7999) + "\x00\n", "a\n", "b\n"},
		{"NUL at byte 8000", strings.Repeat("x", 8000) + "\x00\n", "a\n", "b\n"},
		// Found by random search: sides reaching equal text through
		// differently shaped edits, which conflict in diff3 and keep the
		// neighbouring conflicts apart in the default style.
		{"equal text, different edits",
			"c\nb\na\na\na\na\nc\na\na\nc\nc\nc\na\nc\nb\na\nb\n",
			"c\na\nc\nc\na\na\na\nb\nb\nb\nb\n",
			"c\na\nc\na\na\na\nb\nb\nb\nB\nC\nB\nb"},
		{"equal text between conflicts",
			"b\nb\nc\nb\nc\na\nc\nb\nc\nc\nc\na\nc\nd\nc\nb\nb\na\nc\nd\nd\na\nd\nd\nd",
			"a\nd\nc\na\na\nc\nc\na\na\nd\na\nd\na\nd\nd\nb\na\na\nc\nb\nd\na\na\nb",
			"d\nb\nb\nb\nb\nb\nb\nd\na\nc\nb\na\nc\na\na\na\nd"},
	}
	var cases []mergeCase
	for _, c := range tcs {
		for _, l := range []Labels{{"ours", "base", "theirs"}, {}} {
			cases = append(cases, everyStyle(c.name, c.ours, c.base, c.theirs, l)...)
		}
	}
	compareWithGit(t, cases)
}

// TestFileMatchesGitLarge merges 100k-line files: a realistic edit history,
// and unrelated files over a tiny alphabet, which only stay fast because the
// line diff bounds its search.
func TestFileMatchesGitLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("large inputs")
	}
	rng := rand.New(rand.NewPCG(5, 5))
	render := func(ws []string) string { return lines(ws...) }
	base := make([]string, 100000)
	for i := range base {
		base[i] = strconv.Itoa(rng.IntN(1e9))
	}
	edit := func() []string {
		s := append([]string(nil), base...)
		for range 2000 {
			p := rng.IntN(len(s))
			switch rng.IntN(3) {
			case 0:
				s = append(s[:p], append([]string{strconv.Itoa(rng.IntN(1e9))}, s[p:]...)...)
			case 1:
				s = append(s[:p], s[p+1:]...)
			default:
				s[p] = strconv.Itoa(rng.IntN(1e9))
			}
		}
		return s
	}
	noise := func() string {
		ws := make([]string, 100000)
		for i := range ws {
			ws[i] = string(rune('a' + rng.IntN(4)))
		}
		return render(ws)
	}
	var cases []mergeCase
	cases = append(cases, everyStyle("edited 100k lines", render(edit()), render(base), render(edit()), Labels{})...)
	cases = append(cases, mergeCase{"unrelated 100k lines", []byte(noise()), []byte(noise()), []byte(noise()), Options{}})
	compareWithGit(t, cases)
}

func TestFileRefusesBinary(t *testing.T) {
	text := []byte("a\n")
	nul := append(bytes.Repeat([]byte("x"), binaryProbe-1), 0, '\n')
	for _, in := range [][3][]byte{{nul, text, text}, {text, nul, text}, {text, text, nul}} {
		if _, _, err := File(in[0], in[1], in[2], Options{}); !errors.Is(err, ErrBinary) {
			t.Errorf("NUL within the first %d bytes: err = %v, want ErrBinary", binaryProbe, err)
		}
	}
	late := append(bytes.Repeat([]byte("x"), binaryProbe), 0, '\n')
	if _, _, err := File(late, text, text, Options{}); err != nil {
		t.Errorf("NUL after the first %d bytes: %v", binaryProbe, err)
	}
}
