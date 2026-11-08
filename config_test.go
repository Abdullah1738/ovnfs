package main

import (
	"errors"
	"testing"

	"github.com/stencil-hq/ovnfs/manifest"
)

// TestConfigWriteScope checks where a write without --global or --local goes:
// a daemon setting to ~/.o2/ovnfs.yml, the only file that may hold it, any
// other key to the folder's file; and that get exits 1 on an unset key, as
// git config does.
func TestConfigWriteScope(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Chdir(dir)
	for _, args := range [][]string{
		{"config", "set", "proto", "smb"},
		{"config", "set", "rules", "local data/**"},
	} {
		if err := run(args); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
	}
	if d, err := manifest.UserDaemon(home); err != nil || d.Proto != "smb" {
		t.Fatalf("user file: %+v, %v", d, err)
	}
	m, err := manifest.Load(dir, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := m.Rules.Rules[0]; r.Origin != ".o2/ovnfs.yml:2" || r.String() != "local data/**" {
		t.Fatalf("first rule = %s %s", r.Origin, r)
	}
	var code exitCode
	if err := run([]string{"config", "get", "env.image"}); !errors.As(err, &code) || code != 1 {
		t.Fatalf("get of an unset key = %v, want exit status 1", err)
	}
}
