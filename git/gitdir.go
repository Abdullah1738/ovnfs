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

// CopyGitdir copies a private git directory to the new path dst, linking the copy's
// objects to the shared directory instead of copying them. The registry uses it only
// for snapshots taken by builds that saved a whole directory copy per snapshot.
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

// MarkCheckout sets ovnfs.checkout to name in a private git directory's own config, so
// only this directory identifies as an ovnfs checkout (inherited user config and the
// source repo stay unmarked). The registry calls it whenever it creates or reopens a
// checkout; it edits the file in place of exec'ing git config, keeps every other line,
// and leaves the file untouched when the marker already names the checkout.
func MarkCheckout(gitdir, name string) error {
	path := filepath.Join(gitdir, "config")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	marked := markConfig(string(data), name)
	if marked == string(data) {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(gitdir, ".config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.WriteString(marked)
	if err == nil {
		err = f.Chmod(info.Mode().Perm())
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// markConfig rewrites git config text so its [ovnfs] section holds exactly one
// checkout = name, placed right after the first [ovnfs] header (or in a new section
// at the end); every other line, comments and subsections included, is kept.
func markConfig(config, name string) string {
	var out []string
	insert, inSection, continued := -1, false, false
	for _, line := range strings.SplitAfter(config, "\n") {
		if line == "" {
			continue
		}
		if continued {
			continued = continuesLine(line)
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			end := strings.IndexByte(trimmed, ']')
			if end < 0 {
				inSection = false
				out = append(out, line)
				continue
			}
			inSection = strings.EqualFold(strings.TrimSpace(trimmed[1:end]), "ovnfs")
			if inSection && isCheckoutKey(trimmed[end+1:]) {
				// "[ovnfs] checkout = old" keeps its header and loses the old value.
				continued = continuesLine(line)
				line = trimmed[:end+1] + "\n"
			}
			out = append(out, line)
			if inSection && insert < 0 {
				insert = len(out)
			}
			continue
		}
		if inSection && isCheckoutKey(trimmed) {
			continued = continuesLine(line)
			continue
		}
		out = append(out, line)
	}
	entry := "\tcheckout = " + configValue(name) + "\n"
	if insert < 0 {
		if n := len(out); n != 0 && !strings.HasSuffix(out[n-1], "\n") {
			out[n-1] += "\n"
		}
		out = append(out, "[ovnfs]\n", entry)
	} else {
		out = append(out[:insert], append([]string{entry}, out[insert:]...)...)
	}
	return strings.Join(out, "")
}

// isCheckoutKey reports whether a config line inside a section sets the checkout variable.
func isCheckoutKey(line string) bool {
	line = strings.TrimSpace(line)
	key := line
	if i := strings.IndexAny(line, "= \t"); i >= 0 {
		key = line[:i]
	}
	return strings.EqualFold(key, "checkout")
}

// continuesLine reports whether a config line ends in an unescaped backslash, which
// continues its value on the next line.
func continuesLine(line string) bool {
	line = strings.TrimRight(line, "\r\n")
	n := len(line) - len(strings.TrimRight(line, `\`))
	return n%2 == 1
}

// configValue spells a git config value, quoting and escaping it when git would
// otherwise trim, split or misread it.
func configValue(v string) string {
	if v != "" && v == strings.TrimSpace(v) && !strings.ContainsAny(v, "\"\\#;\n\t") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(v) + `"`
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
