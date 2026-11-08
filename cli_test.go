package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/stencil-hq/ovnfs/api"
)

func TestPrintCheckoutsOrphanSnapshots(t *testing.T) {
	for _, live := range []bool{false, true} {
		r := api.LsReply{Snapshots: []api.Snapshot{
			{ID: "orphan-id", Checkout: "removed", Label: "saved"},
		}}
		if live {
			r.Checkouts = []api.Checkout{{Name: "active"}}
			r.Snapshots = append(r.Snapshots, api.Snapshot{ID: "live-id", Checkout: "active"})
		}
		var out strings.Builder
		printCheckouts(&out, r)
		got := out.String()
		if !strings.Contains(got, "ORPHAN SNAPSHOT") || !hasRow(got, "orphan-id", "removed", "saved") {
			t.Fatalf("orphan missing from %q", got)
		}
		if strings.Contains(got, "live-id") || live && strings.Count(got, "active") != 1 {
			t.Fatalf("live checkout duplicated in %q", got)
		}
	}
}

// TestTablePadding checks that columns are padded by visible width, so a styled
// cell aligns with the plain text a script would see, and that a writer without
// colour gets no escapes at all.
func TestTablePadding(t *testing.T) {
	var plain strings.Builder
	table(&plain, [][]string{{"ID", "NAME"}, {"3f2a", "app"}})
	if got, want := plain.String(), "ID    NAME\n3f2a  app\n"; got != want {
		t.Fatalf("plain table = %q, want %q", got, want)
	}

	var styled strings.Builder
	w := &colorprofile.Writer{Forward: &styled, Profile: colorprofile.ANSI}
	table(w, [][]string{{paint(w, styleBold, "ID"), "NAME"}, {paint(w, styleAccent, "3f2a"), "app"}})
	lines := strings.Split(strings.TrimSuffix(styled.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "\x1b") {
		t.Fatalf("styled table = %q, want two coloured lines", styled.String())
	}
	for i, want := range []string{"ID    NAME", "3f2a  app"} {
		if got := lipgloss.Width(lines[i]); got != lipgloss.Width(want) {
			t.Fatalf("styled row %d is %d wide, want %d: %q", i, got, lipgloss.Width(want), lines[i])
		}
	}
}

// hasRow reports whether some line of out holds every field, in order.
func hasRow(out string, fields ...string) bool {
	for line := range strings.Lines(out) {
		at := 0
		for _, field := range fields {
			i := strings.Index(line[at:], field)
			if i < 0 {
				at = -1
				break
			}
			at += i + len(field)
		}
		if at >= 0 {
			return true
		}
	}
	return false
}

// TestParseInterspersed checks that flags are found anywhere among positionals and "--" ends flag parsing.
func TestParseInterspersed(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		pos      []string
		checkout string
		rules    []string
		json     bool
		err      bool
	}{
		{name: "flags after positional", args: []string{"app", "--name", "b"}, pos: []string{"app"}, checkout: "b"},
		{name: "flags before positional", args: []string{"-name=b", "app"}, pos: []string{"app"}, checkout: "b"},
		{name: "flags between positionals", args: []string{"app", "--json", "snap1", "--rule", "cache x", "--rule", "local y"}, pos: []string{"app", "snap1"}, json: true, rules: []string{"cache x", "local y"}},
		{name: "double dash ends flags", args: []string{"app", "--", "--name", "x"}, pos: []string{"app", "--name", "x"}},
		{name: "lone dash is positional", args: []string{"-", "--json"}, pos: []string{"-"}, json: true},
		{name: "no args", args: nil, pos: nil},
		{name: "unknown flag", args: []string{"app", "--nope"}, err: true},
		{name: "missing value", args: []string{"app", "--name"}, err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cli{}
			fs := c.flags("test")
			var checkout string
			var rules stringList
			fs.StringVar(&checkout, "name", "", "")
			fs.Var(&rules, "rule", "")
			pos, err := parseInterspersed(fs, tt.args)
			if tt.err {
				if err == nil {
					t.Fatalf("parse %q: want error, got positionals %q", tt.args, pos)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", tt.args, err)
			}
			if !slices.Equal(pos, tt.pos) || checkout != tt.checkout || !slices.Equal(rules, tt.rules) || c.json != tt.json {
				t.Fatalf("parse %q = pos %q name %q rules %q json %v; want %q %q %q %v", tt.args, pos, checkout, rules, c.json, tt.pos, tt.checkout, tt.rules, tt.json)
			}
		})
	}
}

// TestParseInterspersedHelp keeps -h distinguishable so verbs can print their usage.
func TestParseInterspersedHelp(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := parseInterspersed(fs, []string{"app", "-h"}); err != flag.ErrHelp {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}
}

// TestResolveTarget checks DIR-versus-CHECKOUT detection and that dirs come back absolute.
func TestResolveTarget(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "notes"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmp)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(filepath.Dir(cwd), "abs", "where")
	tests := []struct {
		arg      string
		dir      string
		checkout string
	}{
		{arg: "", dir: cwd},
		{arg: ".", dir: cwd},
		{arg: "..", dir: filepath.Dir(cwd)},
		{arg: "app", dir: filepath.Join(cwd, "app")},  // existing directory wins over a checkout name
		{arg: "web/", dir: filepath.Join(cwd, "web")}, // a slash means a path even when missing
		{arg: abs, dir: abs},
		{arg: "notes", checkout: "notes"}, // a regular file is not a directory
		{arg: "web", checkout: "web"},     // missing, no slash: a checkout
	}
	for _, tt := range tests {
		dir, checkout, err := resolveTarget(tt.arg)
		if err != nil {
			t.Fatalf("resolveTarget(%q): %v", tt.arg, err)
		}
		if dir != tt.dir || checkout != tt.checkout {
			t.Errorf("resolveTarget(%q) = dir %q checkout %q; want dir %q checkout %q", tt.arg, dir, checkout, tt.dir, tt.checkout)
		}
	}
}

// TestResolveDirs checks the precedence of the daemon's folders: flag, then
// environment, then ~/.o2/ovnfs.yml, then the defaults.
func TestResolveDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("OVNFS_STATE", "")
	t.Setenv("OVNFS_MOUNT", "")
	resolve := func(flagState string) cli {
		t.Helper()
		c := cli{state: flagState}
		if err := c.resolveDirs(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := resolve(""); c.state != defaultState || c.mount != defaultMount {
		t.Errorf("no config = %q %q", c.state, c.mount)
	}
	if err := os.MkdirAll(filepath.Join(home, ".o2"), 0o755); err != nil {
		t.Fatal(err)
	}
	confMount := filepath.Join(t.TempDir(), "conf-mount")
	if err := os.WriteFile(filepath.Join(home, ".o2", "ovnfs.yml"), []byte("state: ~/conf-state\nmount: "+confMount+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := resolve(""); c.state != filepath.Join(home, "conf-state") || c.mount != confMount {
		t.Errorf("config = %q %q", c.state, c.mount)
	}
	t.Setenv("OVNFS_STATE", "/env/state")
	if c := resolve(""); c.state != "/env/state" || c.mount != confMount {
		t.Errorf("env over config = %q %q", c.state, c.mount)
	}
	if c := resolve("/flag/state"); c.state != "/flag/state" {
		t.Errorf("flag over env = %q", c.state)
	}
}

// TestSecondsFlag checks the --idle units: Go durations plus whole days and weeks,
// refusing negatives, fractions of days and durations a time.Duration cannot hold.
func TestSecondsFlag(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want float64
		bad  bool
	}{
		{in: "90m", want: 5400},
		{in: "7d", want: 7 * 86400},
		{in: "2w", want: 14 * 86400},
		{in: "0", want: 0},
		{in: "-1h", bad: true},
		{in: "1.5d", bad: true},
		{in: "5000w", bad: true},
		{in: "d", bad: true},
		{in: "", bad: true},
	} {
		var s seconds
		err := s.Set(tt.in)
		if (err != nil) != tt.bad || !tt.bad && float64(s) != tt.want {
			t.Errorf("Set(%q) = %v, %v; want %v, error %v", tt.in, float64(s), err, tt.want, tt.bad)
		}
	}
}
