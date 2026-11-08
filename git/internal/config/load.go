package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// maxIncludeDepth is how deep includes may nest before git assumes a cycle.
const maxIncludeDepth = 10

// systemConfig is the system-wide file. git's real location is
// $(prefix)/etc/gitconfig for its install prefix (e.g. /opt/homebrew/etc on
// Homebrew), which we cannot know; we use the conventional /etc/gitconfig,
// and on Windows no system file unless GIT_CONFIG_SYSTEM names one.
func systemConfig() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "/etc/gitconfig"
}

// errRemoteURLInHasconfig is git's refusal to let files behind a
// hasconfig:remote.*.url condition influence the condition itself.
var errRemoteURLInHasconfig = errors.New("remote URLs cannot be configured in file directly or indirectly included by includeIf.hasconfig:remote.*.url")

// Options describes the repository whose configuration is loaded and the
// process environment.
type Options struct {
	// GitDir is the repository's git directory ($GIT_DIR; worktrees/<id> in
	// a linked worktree), matched by includeIf gitdir: and holding
	// config.worktree. Empty outside a repository.
	GitDir string
	// CommonDir is the directory holding the repository's config
	// ($GIT_COMMON_DIR). Empty means no repository: no local or worktree scope.
	CommonDir string
	// WorkTree is the top of the working tree, matched by includeIf
	// worktree:. Empty for a bare repository or none.
	WorkTree string
	// LookupEnv reads the environment (HOME, XDG_CONFIG_HOME, GIT_CONFIG_*);
	// nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Branch returns the full ref name HEAD points to ("refs/heads/main"),
	// also for an unborn branch, or "" when HEAD is detached; it is called
	// at most once, only for includeIf onbranch:. Nil means never on a branch.
	Branch func() (string, error)
}

// Load builds the configuration git sees for opts, reading in git's order:
// system, global, local, worktree, then command-line settings from the
// environment, and following include and includeIf directives.
func Load(opts Options) (*Config, error) {
	return load(opts, true)
}

// LoadProtected reads only the protected scopes (system, global and
// command), which git consults for security-sensitive settings such as
// safe.directory before it trusts a repository.
func LoadProtected(opts Options) (*Config, error) {
	return load(opts, false)
}

// source is one top-level configuration source.
type source struct {
	scope Scope
	file  string     // "" for command-line settings
	raw   []rawEntry // command-line settings
	// lenient sources (the global files) are skipped when unreadable, not
	// only when missing.
	lenient bool
}

type loader struct {
	opts    Options
	env     func(string) (string, bool)
	cfg     *Config
	sources []source
	parsed  map[string]parsedFile
	seen    map[string]bool

	scanned bool
	urls    []string // remote.*.url values, for hasconfig:

	branch     string
	branchDone bool
}

type parsedFile struct {
	raw    []rawEntry
	exists bool
	err    error
}

func load(opts Options, repository bool) (*Config, error) {
	env := opts.LookupEnv
	if env == nil {
		env = os.LookupEnv
	}
	l := &loader{opts: opts, env: env, cfg: newConfig(env), parsed: map[string]parsedFile{}, seen: map[string]bool{}}
	if err := l.plan(repository); err != nil {
		return nil, err
	}
	for _, s := range l.sources {
		raw, ok, err := l.sourceEntries(s)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := l.walk(s.scope, s.file, raw, 0, walkMode{}); err != nil {
			return nil, err
		}
	}
	return l.cfg, nil
}

// plan lists the sources to read, in order.
func (l *loader) plan(repository bool) error {
	noSystem := false
	if v, ok := l.env("GIT_CONFIG_NOSYSTEM"); ok {
		b, err := ParseBool(v)
		if err != nil {
			return fmt.Errorf("bad boolean environment value '%s' for 'GIT_CONFIG_NOSYSTEM'", v)
		}
		noSystem = b
	}
	if !noSystem {
		file := systemConfig()
		if v, ok := l.env("GIT_CONFIG_SYSTEM"); ok {
			file = v
		}
		if file != "" {
			l.sources = append(l.sources, source{scope: ScopeSystem, file: file})
		}
	}

	if v, ok := l.env("GIT_CONFIG_GLOBAL"); ok {
		if v != "" {
			l.sources = append(l.sources, source{scope: ScopeGlobal, file: v, lenient: true})
		}
	} else {
		home, haveHome := homeDir(l.env)
		if xdg, ok := l.env("XDG_CONFIG_HOME"); ok && xdg != "" {
			l.sources = append(l.sources, source{scope: ScopeGlobal, file: filepath.Join(xdg, "git", "config"), lenient: true})
		} else if haveHome {
			l.sources = append(l.sources, source{scope: ScopeGlobal, file: filepath.Join(home, ".config", "git", "config"), lenient: true})
		}
		if haveHome {
			l.sources = append(l.sources, source{scope: ScopeGlobal, file: filepath.Join(home, ".gitconfig"), lenient: true})
		}
	}

	if repository && l.opts.CommonDir != "" {
		local := filepath.Join(l.opts.CommonDir, "config")
		l.sources = append(l.sources, source{scope: ScopeLocal, file: local})
		on, err := l.worktreeConfig(local)
		if err != nil {
			return err
		}
		if on && l.opts.GitDir != "" {
			l.sources = append(l.sources, source{scope: ScopeWorktree, file: filepath.Join(l.opts.GitDir, "config.worktree")})
		}
	}

	cmd, err := commandEntries(l.env)
	if err != nil {
		return fmt.Errorf("unable to parse command-line config: %w", err)
	}
	if len(cmd) > 0 {
		l.sources = append(l.sources, source{scope: ScopeCommand, raw: cmd})
	}
	return nil
}

// worktreeConfig reports extensions.worktreeConfig as git decides it: from
// the repository's own config file only, ignoring includes and other scopes.
func (l *loader) worktreeConfig(local string) (bool, error) {
	f := l.read(local, false)
	if f.err != nil {
		return false, f.err
	}
	on := false
	for _, r := range f.raw {
		if r.key != "extensions.worktreeconfig" {
			continue
		}
		on = true
		if r.hasValue {
			b, err := ParseBool(r.value)
			if err != nil {
				return false, fmt.Errorf("bad boolean config value '%s' for 'extensions.worktreeconfig'", r.value)
			}
			on = b
		}
	}
	return on, nil
}

func (l *loader) sourceEntries(s source) ([]rawEntry, bool, error) {
	if s.file == "" {
		return s.raw, true, nil
	}
	l.consult(s.file)
	f := l.read(s.file, s.lenient)
	return f.raw, f.exists, f.err
}

// consult records file in Files.
func (l *loader) consult(file string) {
	if !l.seen[file] {
		l.seen[file] = true
		l.cfg.files = append(l.cfg.files, file)
	}
}

// read parses a config file once per load. A missing file (or, when
// lenient, an unreadable one) yields exists=false and no error.
func (l *loader) read(file string, lenient bool) parsedFile {
	if f, ok := l.parsed[file]; ok {
		return f
	}
	var f parsedFile
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		f.exists = true
		f.raw, f.err = parse(data, file)
	case errors.Is(err, fs.ErrNotExist), lenient && errors.Is(err, fs.ErrPermission):
	default:
		f.err = fmt.Errorf("unable to access '%s': %w", file, err)
	}
	l.parsed[file] = f
	return f
}

// walkMode distinguishes the real pass from the hasconfig pre-scan.
type walkMode struct {
	// scan collects remote URLs without recording entries.
	scan bool
	// forbidURLs rejects remote.*.url, inside files reached through a
	// hasconfig condition (and, during the scan, any includeIf).
	forbidURLs bool
}

func (l *loader) walk(scope Scope, file string, raw []rawEntry, depth int, mode walkMode) error {
	for _, r := range raw {
		section, sub, hasSub, name := splitKey(r.key)
		if section == "remote" && hasSub && name == "url" {
			if mode.forbidURLs {
				return errRemoteURLInHasconfig
			}
			if mode.scan && r.hasValue {
				l.urls = append(l.urls, r.value)
			}
		}
		e := Entry{Key: r.key, Value: r.value, HasValue: r.hasValue, File: file, Line: r.line, Scope: scope}
		if !mode.scan {
			l.cfg.add(e)
		}
		if name != "path" {
			continue
		}
		next := mode
		switch {
		case section == "include" && !hasSub:
		case section == "includeif" && hasSub:
			ok, hasconfig, err := l.condition(sub, file, mode.scan)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			// git's scan cannot tell which includeIf files a hasconfig
			// result might pull in, so it refuses remote URLs in all of them.
			next.forbidURLs = mode.forbidURLs || hasconfig || mode.scan
		default:
			continue
		}
		if err := l.include(e, depth, next); err != nil {
			return err
		}
	}
	return nil
}

func missingValue(e Entry) error {
	msg := fmt.Sprintf("missing value for '%s'", e.Key)
	if e.File == "" {
		return errors.New(msg)
	}
	return &ParseError{File: e.File, Line: e.Line, Reason: msg}
}

func (l *loader) include(e Entry, depth int, mode walkMode) error {
	if !e.HasValue {
		return missingValue(e)
	}
	p, err := expandTilde(e.Value, l.env)
	if err != nil {
		return fmt.Errorf("could not expand include path '%s': %w", e.Value, err)
	}
	if !isAbs(p) {
		if e.File == "" {
			return errors.New("relative config includes must come from files")
		}
		// git joins the includer's directory and the value textually: ".."
		// after a symlink is left for the OS to resolve, and an empty value
		// names the directory itself (an error to read).
		p = textualDir(e.File) + p
	}
	l.consult(p)
	f := l.read(p, false)
	if f.err != nil {
		return f.err
	}
	if !f.exists {
		return nil
	}
	if depth >= maxIncludeDepth {
		from := e.File
		if from == "" {
			from = "command line"
		}
		return fmt.Errorf("exceeded maximum include depth (%d) while including %s from %s; this might be due to circular includes", maxIncludeDepth, p, from)
	}
	return l.walk(e.Scope, p, f.raw, depth+1, mode)
}

// condition evaluates an includeIf condition (the subsection of
// includeIf.<condition>.path). hasconfig reports a hasconfig:remote.*.url
// condition. Unknown keywords are false, as in git.
func (l *loader) condition(cond, file string, scan bool) (ok, hasconfig bool, err error) {
	keyword, pattern, found := strings.Cut(cond, ":")
	if !found {
		return false, false, nil
	}
	switch keyword {
	case "gitdir", "gitdir/i":
		ok, err = l.matchDir(pattern, file, l.opts.GitDir, keyword == "gitdir/i", true)
	case "worktree", "worktree/i":
		ok, err = l.matchDir(pattern, file, l.opts.WorkTree, keyword == "worktree/i", false)
	case "onbranch":
		ok, err = l.onBranch(pattern)
	case "hasconfig":
		rest, isRemote := strings.CutPrefix(pattern, "remote.*.url:")
		if !isRemote {
			return false, false, nil
		}
		if scan {
			// While scanning, follow the include so remote URLs in it are
			// caught, whatever the outcome.
			return true, true, nil
		}
		if err := l.scan(); err != nil {
			return false, true, err
		}
		for _, u := range l.urls {
			if globMatch(rest, u, false) {
				return true, true, nil
			}
		}
		return false, true, nil
	case "hostname":
		host, herr := os.Hostname()
		ok = herr == nil && globMatch(pattern, host, false) // no hostname: no match
	}
	return ok, false, err
}

// matchDir evaluates gitdir:/worktree: patterns against dir. The pattern is
// matched against dir as given and, when spelledToo is false, only against
// its real path; git matches gitdir both ways but worktree only resolved.
func (l *loader) matchDir(pattern, file, dir string, fold, spelledToo bool) (bool, error) {
	if dir == "" {
		return false, nil
	}
	switch {
	case strings.HasPrefix(pattern, "~/"):
		home, ok := homeDir(l.env)
		if !ok {
			return false, fmt.Errorf("failed to expand user dir in: '%s'", pattern)
		}
		pattern = filepath.ToSlash(home) + pattern[1:]
	case strings.HasPrefix(pattern, "./"):
		if file == "" {
			return false, errors.New("relative config include conditionals must come from files")
		}
		pattern = filepath.ToSlash(realDir(file)) + pattern[1:]
	case !isAbs(pattern):
		pattern = "**/" + pattern
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	if spelledToo && globMatch(pattern, filepath.ToSlash(abs), fold) {
		return true, nil
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false, nil // a vanished directory matches nothing
	}
	return globMatch(pattern, filepath.ToSlash(real), fold), nil
}

func (l *loader) onBranch(pattern string) (bool, error) {
	if !l.branchDone {
		l.branchDone = true
		if l.opts.Branch != nil {
			ref, err := l.opts.Branch()
			if err != nil {
				return false, err
			}
			l.branch, _ = strings.CutPrefix(ref, "refs/heads/")
			if l.branch == ref {
				l.branch = "" // detached, or not a branch
			}
		}
	}
	if l.branch == "" {
		return false, nil
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	return globMatch(pattern, l.branch, false), nil
}

// scan collects every remote.*.url from all sources, once, for hasconfig:
// conditions. As in git it follows includes (evaluating other conditions
// normally) but fails if a file behind an includeIf sets a remote URL.
func (l *loader) scan() error {
	if l.scanned {
		return nil
	}
	l.scanned = true
	for _, s := range l.sources {
		raw, ok, err := l.sourceEntries(s)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := l.walk(s.scope, s.file, raw, 0, walkMode{scan: true}); err != nil {
			return err
		}
	}
	return nil
}

// separators are the characters ending a directory in a path: '/' everywhere,
// and the native separator ('\' on Windows).
const separators = string(filepath.Separator) + "/"

// textualDir returns file's directory with its trailing separator, without
// cleaning; "" when file has no directory part.
func textualDir(file string) string {
	return file[:strings.LastIndexAny(file, separators)+1]
}

// realDir is the directory of file's real path, which is what a "./"
// pattern stands for: git resolves the config file (symlinks included,
// relative to the working directory) before taking its directory.
func realDir(file string) string {
	// Clean (inside filepath.Abs) would resolve "link/.." textually, so
	// symlinks are resolved first, on the path as written.
	if real, err := filepath.EvalSymlinks(file); err == nil {
		file = real
	}
	if abs, err := filepath.Abs(file); err == nil {
		file = abs
		// The working directory may itself be spelled through a symlink.
		if real, err := filepath.EvalSymlinks(file); err == nil {
			file = real
		}
	}
	return filepath.Dir(file)
}

// homeDir returns $HOME (set but empty counts), falling back to
// %USERPROFILE% on Windows as Git for Windows does.
func homeDir(env func(string) (string, bool)) (string, bool) {
	if h, ok := env("HOME"); ok {
		return h, true
	}
	if runtime.GOOS == "windows" {
		return env("USERPROFILE")
	}
	return "", false
}

// isAbs reports whether p is absolute for include and pattern purposes: a
// native absolute path or one starting with '/'.
func isAbs(p string) bool {
	return path.IsAbs(p) || filepath.IsAbs(p)
}
