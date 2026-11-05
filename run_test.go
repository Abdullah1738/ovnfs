package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestRunRelativeDir checks root, nested and outside callers without prefix confusion.
func TestRunRelativeDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", "with spaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		cwd  string
		want string
	}{
		{"root", root, "."},
		{"subdirectory", filepath.Join(root, "src", "with spaces"), filepath.Join("src", "with spaces")},
		{"outside", filepath.Dir(root), "."},
		{"similar prefix", root + "-other", "."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := runRelativeDir(root, tt.cwd); got != tt.want {
				t.Fatalf("relative directory = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRunRelativeDirSymlink matches a caller reached through a symlink to its source.
func TestRunRelativeDirSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "source")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if got := runRelativeDir(root, filepath.Join(link, "nested")); got != "nested" {
		t.Fatalf("relative directory = %q, want nested", got)
	}
}

// TestParseRun preserves command flags while forwarding all new options unchanged.
func TestParseRun(t *testing.T) {
	cmd, _ := lookupCommand("run")
	c := &cli{cmd: cmd}
	command := []string{"tool", "--name", "not-a-checkout", "--", "", "$HOME"}
	args := []string{"folder", "--name", "lane", "--rev", "HEAD", "--take", ":staged", "--take", ".:dirty", "--rule", "cache out", "--keep", "--on", "user@host", "--remote-bin", "/with spaces/ovnfs", "--"}
	o, err := parseRun(c, append(args, command...))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.command, command) || !slices.Equal(o.new.Take, []string{":staged", ".:dirty"}) || !slices.Equal(o.new.Rules, []string{"cache out"}) {
		t.Fatalf("lost command or repeatable flags: %+v", o)
	}
	if o.new.Dir != filepath.Join(mustGetwd(t), "folder") || o.new.Name != "lane" || o.new.Rev != "HEAD" || !o.keep || o.on != "user@host" || o.remoteBin != "/with spaces/ovnfs" {
		t.Fatalf("lost run flags: %+v", o)
	}
}

// TestParseRunMissingCommand rejects ambiguous or empty command lines before opening a checkout.
func TestParseRunMissingCommand(t *testing.T) {
	cmd, _ := lookupCommand("run")
	for _, args := range [][]string{nil, {"--"}, {"folder", "--"}, {"echo"}, {"folder", "extra", "--", "echo"}} {
		if _, err := parseRun(&cli{cmd: cmd}, args); err == nil {
			t.Errorf("parse %q succeeded without one DIR and an explicit command", args)
		}
	}
}

// mustGetwd obtains an absolute expected path for flag parsing assertions.
func mustGetwd(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}

// TestRunRemoteCommand checks spaces, apostrophes, dollars and empty arguments through a shell.
func TestRunRemoteCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("quotes for a remote POSIX login shell, which Windows lacks locally")
	}
	dir := filepath.Join(t.TempDir(), "a 'directory' $HOME")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"two words", "it's quoted", "$HOME", "", "$(exit 9)", "line\nbreak", "back\\slash", "\"double\""}
	command := append([]string{"printf", "%s\\000"}, args...)
	got, err := exec.Command("sh", "-c", runRemoteCommand(dir, command)).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for _, arg := range args {
		want = append(want, arg...)
		want = append(want, 0)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
	real, err := filepath.EvalSymlinks(dir) // macOS temp dirs live under the /var symlink
	if err != nil {
		t.Fatal(err)
	}
	got, err = exec.Command("sh", "-c", runRemoteCommand(dir, []string{"pwd", "-P"})).Output()
	if err != nil || string(got) != real+"\n" {
		t.Fatalf("remote cwd = %q, %v; want %q", got, err, real)
	}
}

// TestRunChildForwardSignal delivers a pending CLI termination signal to the child.
func TestRunChildForwardSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows processes cannot be sent SIGTERM; Ctrl+C reaches the child's console directly")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	err := runChild(exec.CommandContext(ctx, "sleep", "30"), signals)
	var code exitCode
	if !errors.As(err, &code) || code != 143 {
		t.Fatalf("forwarded SIGTERM exit = %v, want 143", err)
	}
}

// TestRunChildStatus retains failures, signal deaths and command-not-found status.
func TestRunChildStatus(t *testing.T) {
	failure := exec.Command("sh", "-c", "exit 3")
	if runtime.GOOS == "windows" {
		failure = exec.Command("cmd", "/c", "exit 3")
	}
	for _, tt := range []struct {
		name string
		cmd  *exec.Cmd
		want exitCode
	}{
		{"failure", failure, 3},
		{"signal", exec.Command("sh", "-c", "kill -TERM $$"), 143},
		{"not found", exec.Command(filepath.Join(t.TempDir(), "missing")), 127},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "signal" && runtime.GOOS == "windows" {
				t.Skip("Windows has no signal deaths")
			}
			err := runChild(tt.cmd, nil)
			var code exitCode
			if !errors.As(err, &code) || code != tt.want {
				t.Fatalf("exit = %v, want %d", err, tt.want)
			}
		})
	}
}
