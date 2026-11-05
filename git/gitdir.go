package git

import (
	"context"
	"crypto/sha1"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A checkout of a repository gets a private git directory instead of a worktree of
// the user's repository: HEAD, refs, config and the index are copies taken at
// capture, and "objects" links to an object directory shared by every checkout on
// the same base, which borrows the user's objects through alternates. Commits
// made in a checkout land there; the user's repository is never written.

// PrepareObjects creates a base's shared object directory that reads the repository's objects through alternates.
func (r *Repo) PrepareObjects(dir string) error {
	for _, sub := range []string{"info", "pack"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "info", "alternates"), []byte(filepath.Join(r.CommonDir, "objects")+"\n"), 0o644)
}

// MakeGitdir writes a checkout's private git directory from the repository's current
// HEAD, refs and config, the captured index, and a link to the shared objects.
func (r *Repo) MakeGitdir(ctx context.Context, gitdir, objects, index string) error {
	if err := os.Mkdir(gitdir, 0o755); err != nil {
		return err
	}
	copies := []struct {
		src, dst string
		required bool
	}{
		{filepath.Join(r.GitDir, "HEAD"), "HEAD", true},
		{filepath.Join(r.CommonDir, "config"), "config", true},
		{filepath.Join(r.GitDir, "config.worktree"), "config.worktree", false},
		{filepath.Join(r.CommonDir, "packed-refs"), "packed-refs", false},
		{filepath.Join(r.CommonDir, "shallow"), "shallow", false},
		{filepath.Join(r.CommonDir, "info", "exclude"), filepath.Join("info", "exclude"), false},
		{filepath.Join(r.CommonDir, "refs"), "refs", true},
	}
	for _, c := range copies {
		err := copyTree(c.src, filepath.Join(gitdir, c.dst))
		if err != nil && (c.required || !errors.Is(err, os.ErrNotExist)) {
			return err
		}
	}
	if err := copyFile(index, filepath.Join(gitdir, "index")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := emptyIndex(filepath.Join(gitdir, "index")); err != nil {
			return err
		}
	}
	if err := os.Symlink(objects, filepath.Join(gitdir, "objects")); err != nil {
		return err
	}
	// The checkout is its own work tree (the directory holding the .git file), and its
	// files keep their mtimes but not their inodes, so stat checks stay minimal.
	config := filepath.Join(gitdir, "config")
	for _, args := range [][]string{
		{"--unset-all", "core.worktree"},
		{"--bool", "core.bare", "false"},
		{"core.checkStat", "minimal"},
		{"--bool", "core.trustctime", "false"},
		{"--bool", "core.untrackedCache", "false"},
		{"--bool", "core.fsmonitor", "false"},
	} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"config", "--file", config}, args...)...)
		if _, err := run(cmd); err != nil {
			var exit *exec.ExitError
			// Exit 5 is git config's "nothing to unset".
			if errors.As(err, &exit) && exit.ExitCode() == 5 {
				continue
			}
			return err
		}
	}
	return nil
}

// CopyGitdir copies a checkout's git directory for a fork or snapshot, linking the
// copy's objects to the shared directory instead of copying them.
func CopyGitdir(src, dst, objects string) error {
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "objects" {
			continue
		}
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return os.Symlink(objects, filepath.Join(dst, "objects"))
}

// MarkCheckout identifies only this private git directory as an ovnfs checkout. Using an
// explicit file keeps both inherited user config and the source repo out of it.
func MarkCheckout(ctx context.Context, gitdir, name string) error {
	_, err := run(exec.CommandContext(ctx, "git", "config", "--file", filepath.Join(gitdir, "config"), "ovnfs.checkout", name))
	return err
}

// WriteGitfile points a checkout's root at its private git directory.
func WriteGitfile(root, gitdir string) error {
	if strings.ContainsAny(gitdir, "\r\n") {
		return errors.New("git directory path contains a newline")
	}
	return os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644)
}

// copyTree copies a file, symlink or directory tree to the new path dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return copyFile(p, target)
		}
	})
}

// emptyIndex writes a valid empty version-two index for an unborn repository.
func emptyIndex(name string) error {
	const header = "DIRC\x00\x00\x00\x02\x00\x00\x00\x00"
	checksum := sha1.Sum([]byte(header))
	return os.WriteFile(name, append([]byte(header), checksum[:]...), 0o600)
}
