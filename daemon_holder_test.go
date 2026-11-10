package main

import (
	"path/filepath"
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
