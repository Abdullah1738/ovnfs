package git

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A daemon asks the same repositories the same questions at every capture, and
// on Windows each git process costs tens of milliseconds to start. Answers that
// depend only on files (where a repository lives, what HEAD names, what its
// configuration says) are remembered with the states of those files and reused
// while every one of them is unchanged. A file changed so recently that a later
// change could leave its state equal (racyWindow) keeps its answers from being
// remembered, as git treats racily clean index entries.

// racyWindow covers the coarsest file time resolution in use (FAT's two seconds).
const racyWindow = 2 * time.Second

// fileState is what Lstat reports of a file, the zero value for a missing one.
type fileState struct {
	exists bool
	mode   fs.FileMode
	size   int64
	mtime  time.Time
}

func stateOf(p string) fileState {
	info, err := os.Lstat(p)
	if err != nil {
		return fileState{}
	}
	return fileState{exists: true, mode: info.Mode(), size: info.Size(), mtime: info.ModTime()}
}

// answer is a remembered value and the file states it was derived from.
type answer struct {
	value string
	deps  map[string]fileState
}

var answers struct {
	mu sync.Mutex
	m  map[string]answer
}

// recall returns the value remembered under key while its files are unchanged.
func recall(key string) (string, bool) {
	answers.mu.Lock()
	a, ok := answers.m[key]
	answers.mu.Unlock()
	if !ok {
		return "", false
	}
	for p, s := range a.deps {
		if stateOf(p) != s {
			return "", false
		}
	}
	return a.value, true
}

// remember stores value under key with the current states of deps, the files it
// was derived from by a computation that began at began, unless one of them
// changed since racyWindow before then.
func remember(key, value string, deps []string, began time.Time) {
	states := make(map[string]fileState, len(deps))
	for _, p := range deps {
		s := stateOf(p)
		if s.exists && !s.mtime.Before(began.Add(-racyWindow)) {
			return
		}
		states[p] = s
	}
	answers.mu.Lock()
	defer answers.mu.Unlock()
	if answers.m == nil {
		answers.m = make(map[string]answer)
	}
	answers.m[key] = answer{value: value, deps: states}
}

// forget drops every remembered answer; tests use it to count git processes.
func forget() {
	answers.mu.Lock()
	answers.m = nil
	answers.mu.Unlock()
}

// environment is the part of the environment that changes what git reads, part
// of every key: an answer holds only for the environment it was given in.
func environment() string {
	var vars []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "GIT_"), name == "HOME", name == "XDG_CONFIG_HOME", name == "USERPROFILE", name == "HOMEDRIVE", name == "HOMEPATH":
			vars = append(vars, kv)
		}
	}
	slices.Sort(vars)
	return strings.Join(vars, "\x00")
}

// openDeps lists the files Open's answer for dir, inside work tree r.Top, derives
// from: the .git entry of every directory from dir up to the top, and a linked work
// tree's commondir file.
func openDeps(dir string, r *Repo) []string {
	var deps []string
	for d := dir; ; d = filepath.Dir(d) {
		deps = append(deps, filepath.Join(d, ".git"))
		if d == r.Top || filepath.Dir(d) == d || !inside(r.Top, d) {
			break
		}
	}
	return append(deps, filepath.Join(r.GitDir, "commondir"))
}

// headDeps lists the files Head's answer derives from: HEAD, the ref it names in
// both the git directory and the common one, and packed-refs. It reports false for
// a repository whose refs are not files (reftable).
func (r *Repo) headDeps() ([]string, bool) {
	if _, err := os.Lstat(filepath.Join(r.CommonDir, "reftable")); err == nil {
		return nil, false
	}
	deps := []string{filepath.Join(r.GitDir, "HEAD"), filepath.Join(r.CommonDir, "packed-refs")}
	data, err := os.ReadFile(deps[0])
	if err != nil {
		return nil, false
	}
	if ref, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: "); ok {
		if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
			return nil, false
		}
		deps = append(deps, filepath.Join(r.GitDir, filepath.FromSlash(ref)), filepath.Join(r.CommonDir, filepath.FromSlash(ref)))
	}
	return deps, true
}

// configDeps lists the files r's configuration is read from: those git reports
// for it now, the user's global files whether or not they exist yet, and HEAD,
// which an includeIf onbranch condition reads. It reports false when git cannot
// tell.
func (r *Repo) configDeps(ctx context.Context) ([]string, bool) {
	key := "config files\x00" + r.GitDir + "\x00" + environment()
	if v, ok := recall(key); ok {
		return strings.Split(v, "\x00"), true
	}
	began := time.Now()
	out, err := r.text(ctx, "config", "--list", "--show-origin", "-z")
	if err != nil {
		return nil, false
	}
	seen := map[string]bool{}
	var deps []string
	add := func(p string) {
		if p = filepath.Clean(p); !seen[p] {
			seen[p] = true
			deps = append(deps, p)
		}
	}
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		origin, ok := strings.CutPrefix(fields[i], "file:")
		if !ok {
			if fields[i] == "command line:" {
				continue
			}
			return nil, false // standard input or a blob: nothing on disk to watch
		}
		if !filepath.IsAbs(origin) {
			origin = filepath.Join(r.Top, origin)
		}
		add(origin)
	}
	add(filepath.Join(r.CommonDir, "config"))
	add(filepath.Join(r.GitDir, "config.worktree"))
	add(filepath.Join(r.GitDir, "HEAD"))
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".gitconfig"))
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		add(filepath.Join(configHome, "git", "config"))
	}
	remember(key, strings.Join(deps, "\x00"), deps, began)
	return deps, true
}

// recalled answers a read-only git query from memory when its files are
// unchanged, else asks git through ask and remembers the answer with the files
// deps lists (false: not to be remembered).
func recalled(key string, ask func() (string, error), deps func() ([]string, bool)) (string, error) {
	key += "\x00" + environment()
	if v, ok := recall(key); ok {
		return v, nil
	}
	began := time.Now()
	value, err := ask()
	if err != nil {
		return value, err
	}
	if files, ok := deps(); ok {
		remember(key, value, files, began)
	}
	return value, nil
}

// configValue answers a git config read for r, remembered while r's
// configuration files are unchanged.
func (r *Repo) configValue(ctx context.Context, args ...string) (string, error) {
	return recalled("config\x00"+r.GitDir+"\x00"+strings.Join(args, "\x00"),
		func() (string, error) { return r.optional(ctx, append([]string{"config"}, args...)...) },
		func() ([]string, bool) { return r.configDeps(ctx) })
}

// inside reports whether p is dir or lies below it.
func inside(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
