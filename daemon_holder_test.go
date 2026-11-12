package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServesStateArgumentForms(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state a")
	other := filepath.Join(filepath.Dir(state), "other")
	for _, row := range []struct {
		args []string
		want bool
	}{
		{[]string{"ovnfs", "serve", "-state", state}, true},
		{[]string{"ovnfs", "serve", "--state", state}, true},
		{[]string{"ovnfs", "serve", "--state=" + state}, true},
		{[]string{"ovnfs", "serve", "-state=" + state}, true},
		{[]string{"ovnfs", "--state", state, "serve"}, true},
		{[]string{"ovnfs", "--state", other, "serve"}, false},
		{[]string{"ovnfs", "--state", state, "new", "serve"}, false},
		{[]string{"ovnfs", "--state", state, "new", "--name", "serve"}, false},
		{[]string{"ovnfs", "serve", "--", "--state", state}, false},
		{[]string{"ovnfs", "serve", "--state", "relative"}, false},
		{[]string{"ovnfs", "serve", "--state"}, false},
	} {
		if got := servesState(row.args, state); got != row.want {
			t.Errorf("%q: got %t want %t", row.args, got, row.want)
		}
	}
}

func TestDaemonPathForms(t *testing.T) {
	for _, state := range []string{".", "state", "./state", "sub" + string(filepath.Separator) + ".." + string(filepath.Separator) + "state", "state a", "state" + string(filepath.Separator), t.TempDir()} {
		t.Run(state, func(t *testing.T) {
			want, err := filepath.Abs(state)
			if err != nil {
				t.Fatal(err)
			}
			absolute, mount, err := daemonPaths(state, "./mount")
			if err != nil || absolute != want || !filepath.IsAbs(mount) {
				t.Fatalf("paths: %q %q %v", absolute, mount, err)
			}
			if !servesState([]string{"ovnfs", "serve", "--state", absolute}, want) {
				t.Fatal("normalized state does not match daemon argv")
			}
		})
	}
}

func stateDirectoryAliases(t *testing.T, state string) map[string]string {
	t.Helper()
	rows := map[string]string{"exact": state, "trailing-separator": state + string(filepath.Separator), "dot": state + string(filepath.Separator) + ".", "parent": state + string(filepath.Separator) + "child" + string(filepath.Separator) + ".."}
	if err := os.MkdirAll(filepath.Join(state, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(state), "state-alias")
	if err := os.Symlink(state, alias); err == nil {
		rows["symlink"] = alias
	} else {
		t.Logf("directory alias unavailable: %v", err)
	}
	if runtime.GOOS == "windows" {
		rows["case"] = strings.ToUpper(state)
		rows["forward-separators"] = strings.ReplaceAll(state, `\`, `/`)
		if strings.HasPrefix(state, `\\`) {
			rows["extended-prefix"] = `\\?\UNC\` + strings.TrimPrefix(state, `\\`)
		} else {
			rows["extended-prefix"] = `\\?\` + state
		}
	}
	return rows
}

func TestServesStateDirectoryIdentity(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state a")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	for label, alias := range stateDirectoryAliases(t, state) {
		t.Run(label, func(t *testing.T) {
			if !servesState([]string{"ovnfs", "serve", "--state", state}, alias) {
				t.Fatalf("same directory missed: %q and %q", state, alias)
			}
			if !servesState([]string{"ovnfs", "serve", "--state", alias}, state) {
				t.Fatalf("reverse alias missed: %q and %q", alias, state)
			}
		})
	}
	other := t.TempDir()
	if servesState([]string{"ovnfs", "serve", "--state", other}, state) {
		t.Fatal("accepted different directory")
	}
	if servesState([]string{"ovnfs", "serve", "--state", state + "-missing"}, state) {
		t.Fatal("accepted absent directory")
	}
}

func TestBorrowedStarterArgumentForms(t *testing.T) {
	state := t.TempDir()
	for _, row := range []struct {
		name  string
		flags []string
		want  bool
	}{
		{"foreground", nil, false},
		{"spawned-separate", []string{"--starter-pid", "123", "--starter-fd", "0"}, true},
		{"spawned-equals", []string{"--starter-pid=123", "--starter-fd=0"}, true},
		{"spawned-single-dash", []string{"-starter-pid=123", "-starter-fd=0"}, true},
		{"zero-parent", []string{"--starter-pid=0"}, false},
		{"negative-parent", []string{"--starter-pid=-1"}, false},
		{"malformed-parent", []string{"--starter-pid=no"}, false},
		{"missing-parent-value", []string{"--starter-pid"}, false},
		{"after-separator", []string{"--", "--starter-pid=123"}, false},
		{"other-command-args", []string{"--starter-pid=123", "other"}, false},
		{"not-windows-lease", []string{"--starter-pid=123", "--starter-fd=3"}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := append([]string{"ovnfs", "serve", "--state", state}, row.flags...)
			if got := borrowsStartForState(args, state); got != row.want {
				t.Fatalf("%v got%t want%t", args, got, row.want)
			}
		})
	}
}
