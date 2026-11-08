package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
)

// errNotGitDir is gitDirLayout's "this directory is not a git directory".
var errNotGitDir = errors.New("not a git directory")

// Discover finds the repository whose work tree contains dir, as
// `git -C <dir> rev-parse --show-toplevel --git-common-dir --git-dir` does,
// and fails unless dir is inside that work tree. All paths are physical:
// symlinks are resolved (on macOS also the case of each component).
//
// From dir upwards, each directory is checked first for a .git entry — a
// valid git directory, or a gitfile ("gitdir: <path>", relative to the
// file's directory) whose target must be valid — and then for being a git
// directory itself (dir inside a .git directory or a bare repository, which
// yields ErrNoWorkTree unless core.worktree contains dir). The search does
// not go up into the deepest GIT_CEILING_DIRECTORIES entry above dir, nor
// cross a filesystem boundary unless GIT_DISCOVERY_ACROSS_FILESYSTEM is true
// (the boundary is not detected on Windows).
//
// A found repository must pass the safe.directory ownership check
// (ErrUnsafe) and the repository format check (ErrUnsupportedFormat); its
// work tree is then core.worktree when set, else the directory holding .git.
func Discover(dir string) (*Repository, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	cwd, err := realPath(abs)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(cwd); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("repo: %s: not a directory", cwd)
	}
	ceiling := ceilingFor(cwd)
	acrossFS := envBool("GIT_DISCOVERY_ACROSS_FILESYSTEM")
	startDev, devOK := device(cwd)
	for cand := cwd; ; {
		dotgit := filepath.Join(cand, ".git")
		if fi, err := os.Stat(dotgit); err == nil {
			switch {
			case fi.Mode().IsRegular():
				gitDir, err := readGitfile(dotgit)
				if err != nil {
					return nil, err
				}
				commonDir, err := gitDirLayout(gitDir)
				if errors.Is(err, errNotGitDir) {
					return nil, fmt.Errorf("repo: %s: %w: does not point to a valid repository", dotgit, ErrInvalidGitfile)
				}
				if err != nil {
					return nil, err
				}
				return found(cwd, cand, dotgit, gitDir, commonDir)
			case fi.IsDir():
				commonDir, err := gitDirLayout(dotgit)
				if err == nil {
					return found(cwd, cand, "", physical(dotgit), commonDir)
				}
				if !errors.Is(err, errNotGitDir) {
					return nil, err
				}
			}
		}
		commonDir, err := gitDirLayout(cand)
		if err == nil {
			return found(cwd, "", "", cand, commonDir)
		}
		if !errors.Is(err, errNotGitDir) {
			return nil, err
		}
		parent := filepath.Dir(cand)
		if parent == cand || parent == ceiling {
			return nil, fmt.Errorf("repo: %s: %w", cwd, ErrNotRepository)
		}
		if !acrossFS && devOK {
			if dev, ok := device(parent); ok && dev != startDev {
				return nil, fmt.Errorf("repo: %s: %w (stopping at filesystem boundary)", cwd, ErrNotRepository)
			}
		}
		cand = parent
	}
}

// found finishes discovery of gitDir from cwd. workDir is the directory
// holding the .git entry ("" when cwd is inside the git directory itself)
// and gitfile the .git file, if that is what led to gitDir.
func found(cwd, workDir, gitfile, gitDir, commonDir string) (*Repository, error) {
	owned := []string{gitDir}
	safeDir := gitDir
	if workDir != "" {
		owned = append(owned, workDir)
		safeDir = workDir
	}
	if gitfile != "" {
		owned = append(owned, gitfile)
	}
	if err := checkOwnership(owned, safeDir, gitDir, workDir); err != nil {
		return nil, err
	}
	s, err := readSettings(gitDir, commonDir)
	if err != nil {
		return nil, err
	}
	if s.bareSet && s.bare {
		return nil, fmt.Errorf("repo: %s: %w (bare repository)", gitDir, ErrNoWorkTree)
	}
	top := workDir
	if s.worktreeSet {
		if top, err = worktreePath(gitDir, s.worktree); err != nil {
			return nil, err
		}
	}
	if top == "" {
		return nil, fmt.Errorf("repo: %s: %w (inside the git directory %s)", cwd, ErrNoWorkTree, gitDir)
	}
	if !within(cwd, top) {
		return nil, fmt.Errorf("repo: %s: %w (outside %s)", cwd, ErrNoWorkTree, top)
	}
	return newRepository(top, gitDir, commonDir, false, s)
}

// gitDirLayout checks that dir is a git directory as git requires before
// using one: a valid HEAD in dir, and "objects" and "refs" directories in its
// common directory, which a "commondir" file names (relative to dir) and
// which is dir itself otherwise. It returns the physical common directory,
// or errNotGitDir.
func gitDirLayout(dir string) (string, error) {
	if !validHead(filepath.Join(dir, "HEAD")) {
		return "", errNotGitDir
	}
	commonDir := dir
	data, err := os.ReadFile(filepath.Join(dir, "commondir"))
	switch {
	case err == nil:
		p := trimEOL(string(data))
		if p == "" {
			return "", fmt.Errorf("repo: %s: empty commondir file", dir)
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		commonDir = p
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	for _, sub := range []string{"objects", "refs"} {
		if fi, err := os.Stat(filepath.Join(commonDir, sub)); err != nil || !fi.IsDir() {
			return "", errNotGitDir
		}
	}
	return physical(commonDir), nil
}

// validHead reports whether path is a HEAD git accepts as marking a git
// directory: a symlink whose target starts with "refs/", or a file holding
// "ref:" (optionally followed by whitespace) and a name starting with
// "refs/", or starting with at least 40 hex digits. Nothing else is checked.
// Git for Windows reads symlink targets with forward slashes.
func validHead(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		return err == nil && strings.HasPrefix(filepath.ToSlash(target), "refs/")
	}
	if !fi.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if rest, ok := strings.CutPrefix(string(data), "ref:"); ok {
		return strings.HasPrefix(strings.TrimLeft(rest, " \t\n\r\v\f"), "refs/")
	}
	if len(data) < 40 {
		return false
	}
	for _, c := range data[:40] {
		if !isHex(c) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// readGitfile returns the physical git directory a .git file points to. The
// file must start with "gitdir: "; the rest, less trailing CR and LF, is the
// path, relative to the file's directory unless absolute. On Windows a path
// with a component ending in a space or period is invalid, as Git for
// Windows refuses to open it (core.protectNTFS) where Windows would strip
// the trailing characters.
func readGitfile(name string) (string, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	rest, ok := strings.CutPrefix(string(data), "gitdir: ")
	if !ok {
		return "", fmt.Errorf("repo: %s: %w: bad format", name, ErrInvalidGitfile)
	}
	p := trimEOL(rest)
	if p == "" {
		return "", fmt.Errorf("repo: %s: %w: no path", name, ErrInvalidGitfile)
	}
	if runtime.GOOS == "windows" && hasTrailingSpaceOrPeriod(p) {
		return "", fmt.Errorf("repo: %s: %w: does not point to a valid repository", name, ErrInvalidGitfile)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(name), p)
	}
	real, err := realPath(p)
	if err != nil {
		return "", fmt.Errorf("repo: %s: %w: does not point to a valid repository", name, ErrInvalidGitfile)
	}
	return real, nil
}

// hasTrailingSpaceOrPeriod reports whether a component of p other than "."
// and ".." ends in a space or a period.
func hasTrailingSpaceOrPeriod(p string) bool {
	for _, c := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if c != "." && c != ".." && (strings.HasSuffix(c, " ") || strings.HasSuffix(c, ".")) {
			return true
		}
	}
	return false
}

// trimEOL drops trailing CR and LF characters, as git does for the one-line
// files of a git directory (gitfile, commondir, gitdir).
func trimEOL(s string) string { return strings.TrimRight(s, "\r\n") }

// worktreePath resolves core.worktree, relative to the git directory.
func worktreePath(gitDir, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("repo: %s: empty core.worktree", gitDir)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(gitDir, value)
	}
	return physical(value), nil
}

// physical returns p's physical path, or p cleaned when it cannot be
// resolved (it does not exist).
func physical(p string) string {
	if real, err := realPath(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// within reports whether path is dir or below it.
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(path, dir)
}

// ceilingFor returns the deepest GIT_CEILING_DIRECTORIES entry that is a
// proper ancestor of cwd, or "". Entries are absolute paths (others are
// ignored) separated by the OS list separator; they are resolved to physical
// paths except after an empty entry, which declares the rest symlink-free.
func ceilingFor(cwd string) string {
	v := os.Getenv("GIT_CEILING_DIRECTORIES")
	resolve := true
	best := ""
	for _, e := range filepath.SplitList(v) {
		if e == "" {
			resolve = false
			continue
		}
		if !filepath.IsAbs(e) {
			continue
		}
		c := filepath.Clean(e)
		if resolve {
			c = physical(c)
		}
		if c != cwd && within(cwd, c) && len(c) > len(best) {
			best = c
		}
	}
	return best
}

// envBool reads a boolean environment variable; unset or invalid is false.
func envBool(name string) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return false
	}
	b, err := config.ParseBool(v)
	return err == nil && b
}
