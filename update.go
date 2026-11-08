package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/stencil-hq/ovnfs/internal/release"
)

// updateCmd replaces this binary with the latest ovnfs, then points out a daemon
// still running the old one.
type updateCmd struct{}

func (cmd *updateCmd) Run(c *cli) error {
	c.autostart = false
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return errors.New("this binary carries no build info to update from")
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}
	updated, err := update(c.ctx, exe, bi)
	if err != nil || !updated {
		return err
	}
	if accepting(c.state) {
		fmt.Fprintln(stderr, paint(stderr, styleYellow, "the running daemon is the old version; 'ovnfs restart' swaps in the new one"))
	}
	return nil
}

// update replaces exe, the binary bi describes, with the latest ovnfs and
// reports whether it changed anything. A binary go install built, which records
// the module's checksum unlike a build in a clone, is rebuilt with go install
// from the latest module version; a release binary is replaced by the latest
// release's from the build service. A build in a clone is refused: pulling and
// rebuilding there updates it.
func update(ctx context.Context, exe string, bi *debug.BuildInfo) (bool, error) {
	current, viaGo := bi.Main.Version, bi.Main.Sum != ""
	if !viaGo && !release.IsTag(current) {
		return false, fmt.Errorf("ovnfs %s was built in a clone; pull and rebuild it there", current)
	}
	var latest string
	var bin release.Binary
	if viaGo {
		out, err := goCmd(ctx, "list", "-m", "-f", "{{.Version}}", bi.Main.Path+"@latest").Output()
		if err != nil {
			return false, fmt.Errorf("go list -m %s@latest: %w", bi.Main.Path, err)
		}
		latest = strings.TrimSpace(string(out))
	} else {
		var err error
		if bin, err = release.Find(ctx, "", runtime.GOOS, runtime.GOARCH); err != nil {
			return false, err
		}
		latest = bin.Tag
	}
	if latest == current {
		fmt.Fprintf(stderr, "%s is the latest\n", paint(stderr, styleGreen, "ovnfs "+current))
		return false, nil
	}
	fmt.Fprintf(stderr, "%s %s from ovnfs %s to %s\n", paint(stderr, styleYellow, "updating"), exe, current, paint(stderr, styleGreen, latest))

	// Stage the new binary beside exe, on the same volume, so it can be renamed over it.
	dir, err := os.MkdirTemp(filepath.Dir(exe), ".ovnfs-update-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	next := filepath.Join(dir, "ovnfs")
	if runtime.GOOS == "windows" {
		next += ".exe"
	}
	if viaGo {
		cmd := goCmd(ctx, "install", bi.Path+"@"+latest)
		cmd.Env = append(os.Environ(), "GOBIN="+dir, "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
		cmd.Stdout = os.Stderr
		if err := cmd.Run(); err != nil {
			return false, fmt.Errorf("go install %s@%s: %w", bi.Path, latest, err)
		}
	} else {
		data, err := bin.Fetch(ctx)
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(next, data, 0o755); err != nil {
			return false, err
		}
	}
	if out, err := exec.CommandContext(ctx, next, "version").CombinedOutput(); err != nil {
		return false, fmt.Errorf("ovnfs %s does not run here: %w\n%s", latest, err, out)
	}
	if err := replaceExe(exe, next); err != nil {
		return false, fmt.Errorf("replacing %s: %w", exe, err)
	}
	fmt.Fprintf(stderr, "%s to %s\n", paint(stderr, styleGreen, "updated"), paint(stderr, styleGreen, "ovnfs "+latest))
	return true, nil
}

// goCmd prepares a go command that prints its progress and errors on stderr and
// runs outside any module, so the requirements and replacements of the one in
// the working directory stay out of its version queries.
func goCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir, cmd.Stderr = os.TempDir(), os.Stderr
	return cmd
}

// replaceExe renames next over exe. Windows refuses to overwrite or remove a
// running executable but lets it be renamed, so there exe first moves aside to
// exe.old, which the next update removes once nothing runs it.
func replaceExe(exe, next string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(next, exe)
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(next, exe); err != nil {
		os.Rename(old, exe)
		return err
	}
	os.Remove(old) // fails while this process or a daemon still runs it
	return nil
}
