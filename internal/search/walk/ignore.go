package walk

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/pattern"
)

// Ignore-file semantics follow the `ignore` crate:
//
//   - Each directory may contribute `.ignore`, `.gitignore`, and (when it has a
//     `.git` entry) `.git/info/exclude`. For each of the three kinds the
//     nearest directory with a matching rule decides; `.ignore` beats
//     `.gitignore` beats the exclude file beats the global excludes file.
//   - Exclude files above the nearest repository root (`.git` or `.jj`) are
//     not consulted; exclude and global rules apply only inside a repository.
//     `.gitignore` applies everywhere.
//   - Ignore files in ancestors of the walk root, up to the nearest
//     repository root, apply too, minus any rule that would ignore the walk
//     root itself.
//   - Within one file the last matching rule wins; `dir/` rules only match
//     directories; `!` rules re-include.

// matchResult is the outcome of matching a path against ignore rules.
type matchResult uint8

const (
	matchNone matchResult = iota
	matchIgnore
	matchWhitelist
)

type rule struct {
	glob      *pattern.Glob
	dirOnly   bool
	whitelist bool
}

// ignoreFile is the compiled rule list of one ignore file.
type ignoreFile struct {
	rules    []rule
	anchored bool // some rule needs the full path, not just the basename
}

// ruleOptions is the glob dialect of gitignore rules: an unclosed '[' is a
// literal, as git and the ignore crate treat it.
var ruleOptions = pattern.Options{AllowUnclosedClass: true}

// maxIgnoreBytes caps how much of one ignore file is read.
const maxIgnoreBytes = 16 << 20

// parseIgnore compiles ignore-file contents, keeping lines accepted by keep
// (nil keeps all). Like the ignore crate it strips one leading BOM run, splits
// on "\n" or "\r\n", skips invalid rules, and stops at the first line that is
// not valid UTF-8. It returns nil when no rule survives.
func parseIgnore(ctx context.Context, data []byte, keep func(line string) bool) *ignoreFile {
	f := &ignoreFile{}
	for first := true; len(data) > 0; first = false {
		if ctx.Err() != nil {
			return nil
		}
		var raw []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			raw, data = data[:i], data[i+1:]
			raw = bytes.TrimSuffix(raw, []byte{'\r'})
		} else {
			raw, data = data, nil
		}
		if !utf8.Valid(raw) {
			break
		}
		line := string(raw)
		if first {
			line = strings.TrimLeft(line, "\uFEFF")
		}
		if keep != nil && !keep(line) {
			continue
		}
		if r, ok := parseRule(line); ok {
			f.rules = append(f.rules, r)
			f.anchored = f.anchored || !r.glob.BasenameOnly()
		}
	}
	if len(f.rules) == 0 {
		return nil
	}
	return f
}

// parseRule ports GitignoreBuilder::add_line.
func parseRule(line string) (rule, bool) {
	var r rule
	if strings.HasPrefix(line, "#") {
		return r, false
	}
	if !strings.HasSuffix(line, "\\ ") {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	if line == "" {
		return r, false
	}
	absolute := false
	if strings.HasPrefix(line, "\\!") || strings.HasPrefix(line, "\\#") {
		line = line[1:]
	} else {
		if strings.HasPrefix(line, "!") {
			r.whitelist = true
			line = line[1:]
		}
		if strings.HasPrefix(line, "/") {
			line = line[1:]
			absolute = true
		}
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line[:len(line)-1], "\\")
	}
	actual := line
	if !absolute && !strings.Contains(line, "/") && !strings.HasPrefix(actual, "**/") && actual != "**" {
		actual = "**/" + actual
	}
	if strings.HasSuffix(actual, "/**") {
		actual += "/*"
	}
	g, err := pattern.Compile(actual, ruleOptions)
	if err != nil {
		return r, false
	}
	r.glob = g
	return r, true
}

// match returns the decision of the last rule matching path, a path relative
// to the ignore file's directory.
func (f *ignoreFile) match(path string, isDir bool) matchResult {
	base := path[strings.LastIndexByte(path, '/')+1:]
	for i := len(f.rules) - 1; i >= 0; i-- {
		r := &f.rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		if r.glob.MatchBase(path, base) {
			if r.whitelist {
				return matchWhitelist
			}
			return matchIgnore
		}
	}
	return matchNone
}

// matchPathOrParents ports Gitignore::matched_path_or_any_parents for a
// directory path: the path itself, then each ancestor down to "".
func (f *ignoreFile) matchPathOrParents(path string) matchResult {
	for {
		if m := f.match(path, true); m != matchNone {
			return m
		}
		if path == "" {
			return matchNone
		}
		path = path[:max(strings.LastIndexByte(path, '/'), 0)]
	}
}

// ignoreState is one directory's frame in the chain of applicable ignore
// files. Frames are immutable and shared by all descendants.
type ignoreState struct {
	parent  *ignoreState
	dot     *ignoreFile // .ignore
	git     *ignoreFile // .gitignore
	exclude *ignoreFile // .git/info/exclude
	// base is the frame directory relative to the walk root ("" for the
	// root). Frames above the root instead set up: the walk root relative to
	// the frame directory.
	base, up         string
	hasGit           bool // the directory is a repository root
	chainHasMatchers bool
	anyGit           bool
}

func newIgnoreState(parent *ignoreState, dot, git, exclude *ignoreFile, hasGit bool) *ignoreState {
	s := &ignoreState{parent: parent, dot: dot, git: git, exclude: exclude, hasGit: hasGit}
	s.chainHasMatchers = dot != nil || git != nil || exclude != nil
	s.anyGit = hasGit
	if parent != nil {
		s.chainHasMatchers = s.chainHasMatchers || parent.chainHasMatchers
		s.anyGit = s.anyGit || parent.anyGit
	}
	return s
}

// entryNames records which ignore-relevant names a listing contains.
type entryNames uint8

const (
	hasDotIgnore entryNames = 1 << iota
	hasGitignore
	hasGitDir
	hasRepoMarker
)

func (n *entryNames) record(name []byte, typ fsys.Type) {
	if len(name) < 3 || name[0] != '.' {
		return
	}
	switch string(name) {
	case ".ignore":
		if typ != fsys.Dir {
			*n |= hasDotIgnore
		}
	case ".gitignore":
		if typ != fsys.Dir {
			*n |= hasGitignore
		}
	case ".git":
		*n |= hasGitDir | hasRepoMarker
	case ".jj":
		*n |= hasRepoMarker
	}
}

// ignoreSources are the per-directory ignore files, in precedence order.
var ignoreSources = [3]string{".ignore", ".gitignore", ".git/info/exclude"}

// IgnoreFiles returns the files below a directory that Options.Gitignore
// reads, so filesystems that fetch content lazily can fetch them ahead of a
// walk.
func IgnoreFiles() [len(ignoreSources)]string { return ignoreSources }

// sourceFlags are the listing names that make each ignore source worth
// reading: the file itself, or a `.git` entry for the exclude file.
var sourceFlags = [3]entryNames{hasDotIgnore, hasGitignore, hasGitDir}

// readIgnoreFile reads up to maxIgnoreBytes of the file name, relative to
// dir (absolute with the zero Handle), into the reusable *buf.
func readIgnoreFile(ctx context.Context, s fsys.Session, buf *[]byte, dir fsys.Handle, name string) ([]byte, fsys.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fsys.FileInfo{}, err
	}
	h, info, err := s.OpenFile(dir, name)
	if err != nil {
		return nil, info, err
	}
	defer s.Close(h)
	if err := ctx.Err(); err != nil {
		return nil, info, err
	}
	*buf, err = s.Read(h, *buf, maxIgnoreBytes)
	if cancelErr := ctx.Err(); cancelErr != nil {
		return nil, info, cancelErr
	}
	return *buf, info, err
}

// dirIgnores holds one directory's compiled ignore sources and, for the
// listing cache, the stamps they were read at (zero when absent).
type dirIgnores struct {
	files  [3]*ignoreFile
	stamps [3]fsys.Stamp
}

// loadIgnores reads the ignore sources a listing with names calls for,
// relative to the open directory dir, into the reusable *buf; stamped also
// records their stamps.
func loadIgnores(ctx context.Context, s fsys.Session, buf *[]byte, dir fsys.Handle, names entryNames, stamped bool) dirIgnores {
	var d dirIgnores
	for k, name := range ignoreSources {
		if ctx.Err() != nil {
			return d
		}
		if names&sourceFlags[k] == 0 {
			continue
		}
		data, info, err := readIgnoreFile(ctx, s, buf, dir, name)
		if err != nil {
			continue
		}
		if stamped {
			d.stamps[k] = info.Stamp
		}
		d.files[k] = parseIgnore(ctx, data, nil)
	}
	return d
}

// state returns the frame for a directory whose listing contained names.
func (d *dirIgnores) state(parent *ignoreState, names entryNames, base string) *ignoreState {
	if names == 0 {
		return parent
	}
	s := newIgnoreState(parent, d.files[0], d.files[1], d.files[2], names&hasRepoMarker != 0)
	s.base = base
	return s
}

// ancestorStates builds the frames for the directories between the nearest
// repository root at or above root's parent and root's parent, dropping
// rules that would ignore root itself. root must be absolute and clean, with
// separator sep; everything is read through s into *buf.
func ancestorStates(ctx context.Context, s fsys.Session, buf *[]byte, root string, sep byte) *ignoreState {
	if ctx.Err() != nil {
		return nil
	}
	dirOf := path.Dir
	if sep != '/' {
		dirOf = filepath.Dir
	}
	join := func(dir, name string) string {
		if !strings.HasSuffix(dir, string(sep)) {
			dir += string(sep)
		}
		return dir + name
	}
	exists := func(p string) bool {
		if ctx.Err() != nil {
			return false
		}
		_, err := s.Stat(p, true)
		return err == nil && ctx.Err() == nil
	}
	var ancestors []string
	repo := -1
	for dir := root; ; {
		if ctx.Err() != nil {
			return nil
		}
		parent := dirOf(dir)
		if parent == dir {
			break
		}
		dir = parent
		ancestors = append(ancestors, dir)
		if repo < 0 && (exists(join(dir, ".git")) || exists(join(dir, ".jj"))) {
			repo = len(ancestors) - 1
		}
	}
	var state *ignoreState
	for i := repo; i >= 0; i-- {
		if ctx.Err() != nil {
			return nil
		}
		dir := ancestors[i]
		up := strings.TrimPrefix(root[len(dir):], string(sep))
		if sep != '/' {
			up = strings.ReplaceAll(up, string(sep), "/")
		}
		hasGit := exists(join(dir, ".git")) || exists(join(dir, ".jj"))
		var exclude *ignoreFile
		if hasGit {
			exclude = loadAncestorIgnore(ctx, s, buf, join(dir, ".git/info/exclude"), up)
		}
		state = newIgnoreState(state,
			loadAncestorIgnore(ctx, s, buf, join(dir, ".ignore"), up),
			loadAncestorIgnore(ctx, s, buf, join(dir, ".gitignore"), up),
			exclude, hasGit)
		state.up = up
	}
	return state
}

func loadAncestorIgnore(ctx context.Context, s fsys.Session, buf *[]byte, p, up string) *ignoreFile {
	data, _, err := readIgnoreFile(ctx, s, buf, fsys.Handle{}, p)
	if err != nil {
		return nil
	}
	f := parseIgnore(ctx, data, nil)
	if f == nil || f.matchPathOrParents(up) != matchIgnore {
		return f
	}
	return parseIgnore(ctx, data, func(line string) bool {
		r, ok := parseRule(line)
		if !ok {
			return true
		}
		single := ignoreFile{rules: []rule{r}}
		return single.matchPathOrParents(up) != matchIgnore
	})
}

// ignorer evaluates ignore chains for one walk.
type ignorer struct {
	global *ignoreFile
	cwd    string // root of the global excludes matcher
	root   string // absolute walk root
}

// ignored reports whether the entry at walk-relative path rel is ignored by
// the chain ending at s. buf is per-worker scratch.
func (ig *ignorer) ignored(s *ignoreState, rel string, isDir bool, buf *[]byte) bool {
	if s == nil {
		return false // no frames: no ignore files and not inside a repository
	}
	globalApplies := s.anyGit && ig.global != nil
	if !s.chainHasMatchers && !globalApplies {
		return false
	}
	var dot, git, exclude matchResult
	sawGit := false
	for f := s; f != nil && f.chainHasMatchers; f = f.parent {
		if dot == matchNone && f.dot != nil {
			dot = f.dot.match(f.relPath(f.dot, rel, buf), isDir)
		}
		if git == matchNone && f.git != nil {
			git = f.git.match(f.relPath(f.git, rel, buf), isDir)
		}
		if s.anyGit && !sawGit && exclude == matchNone && f.exclude != nil {
			exclude = f.exclude.match(f.relPath(f.exclude, rel, buf), isDir)
		}
		sawGit = sawGit || f.hasGit
	}
	for _, m := range [...]matchResult{dot, git, exclude} {
		if m != matchNone {
			return m == matchIgnore
		}
	}
	if globalApplies {
		return ig.global.match(ig.globalPath(rel, buf), isDir) == matchIgnore
	}
	return false
}

// relPath returns rel relative to frame f's directory. Basename-only rule
// files get rel unchanged since only its last component matters.
func (f *ignoreState) relPath(file *ignoreFile, rel string, buf *[]byte) string {
	if !file.anchored {
		return rel
	}
	if f.up != "" {
		*buf = append(append(append((*buf)[:0], f.up...), '/'), rel...)
		return unsafe.String(unsafe.SliceData(*buf), len(*buf))
	}
	if f.base == "" {
		return rel
	}
	return rel[len(f.base)+1:]
}

// globalPath mirrors how the ignore crate strips the global matcher's root
// (the process working directory) from absolute candidate paths.
func (ig *ignorer) globalPath(rel string, buf *[]byte) string {
	if !ig.global.anchored {
		return rel
	}
	*buf = append(append(append((*buf)[:0], ig.root...), '/'), rel...)
	p := unsafe.String(unsafe.SliceData(*buf), len(*buf))
	if ig.cwd != "" && strings.HasPrefix(p, ig.cwd) {
		p = strings.TrimPrefix(p[len(ig.cwd):], "/")
	}
	return p
}

// globalExcludes loads git's global excludes file the way the ignore crate
// locates it: core.excludesFile from $GIT_CONFIG_GLOBAL, ~/.gitconfig,
// $XDG_CONFIG_HOME/git/config, then $GIT_CONFIG_SYSTEM or /etc/gitconfig,
// falling back to $XDG_CONFIG_HOME/git/ignore.
// Config and excludes files are host paths read through s into *buf (see
// Options.Config), so a provider filesystem answers for them like for
// everything else.
func globalExcludes(ctx context.Context, s fsys.Session, buf *[]byte) *ignoreFile {
	if ctx.Err() != nil {
		return nil
	}
	read := func(p string) ([]byte, bool) {
		data, _, err := readIgnoreFile(ctx, s, buf, fsys.Handle{}, p)
		return data, err == nil
	}
	path := globalExcludesPath(read)
	if path == "" {
		return nil
	}
	data, ok := read(path)
	if !ok {
		return nil
	}
	return parseIgnore(ctx, data, nil)
}

func globalExcludesPath(read func(string) ([]byte, bool)) string {
	home := os.Getenv("HOME")
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" && home != "" {
		configHome = filepath.Join(home, ".config")
	}
	var configs []string
	if p := os.Getenv("GIT_CONFIG_GLOBAL"); p != "" {
		configs = append(configs, p)
	}
	if home != "" {
		configs = append(configs, filepath.Join(home, ".gitconfig"))
	}
	if configHome != "" {
		configs = append(configs, filepath.Join(configHome, "git", "config"))
	}
	if p := os.Getenv("GIT_CONFIG_SYSTEM"); p != "" {
		configs = append(configs, p)
	} else {
		configs = append(configs, "/etc/gitconfig")
	}
	for _, config := range configs {
		data, ok := read(config)
		if !ok {
			continue
		}
		if value, ok := parseExcludesFile(data); ok {
			if home != "" {
				value = strings.ReplaceAll(value, "~", home)
			}
			return value
		}
	}
	if configHome == "" {
		return ""
	}
	return filepath.Join(configHome, "git", "ignore")
}

// parseExcludesFile implements the equivalent of the ignore crate's
// `(?im-u)^\s*excludesfile\s*=\s*"?\s*(\S+?)\s*"?\s*$`.
func parseExcludesFile(data []byte) (string, bool) {
	const key = "excludesfile"
	isSpace := func(b byte) bool {
		return b == '\t' || b == '\n' || b == 0x0b || b == 0x0c || b == '\r' || b == ' '
	}
	skipSpace := func(i int) int {
		for i < len(data) && isSpace(data[i]) {
			i++
		}
		return i
	}
	endsLine := func(i int) bool {
		end := skipSpace(i)
		return end == len(data) || bytes.IndexByte(data[i:end], '\n') >= 0
	}
	tailMatches := func(i int) bool {
		quote := skipSpace(i)
		return endsLine(i) || (quote < len(data) && data[quote] == '"' && endsLine(quote+1))
	}
	captureAt := func(start int) (string, bool) {
		for end := start; end < len(data) && !isSpace(data[end]); {
			end++
			if tailMatches(end) {
				return string(data[start:end]), true
			}
		}
		return "", false
	}
	for start := 0; start <= len(data); {
		k := skipSpace(start)
		if k+len(key) <= len(data) && equalFoldASCII(data[k:k+len(key)], key) {
			eq := skipSpace(k + len(key))
			if eq < len(data) && data[eq] == '=' {
				v := skipSpace(eq + 1)
				if v < len(data) && data[v] == '"' {
					if s, ok := captureAt(skipSpace(v + 1)); ok {
						return s, true
					}
				}
				if s, ok := captureAt(v); ok {
					return s, true
				}
			}
		}
		next := bytes.IndexByte(data[start:], '\n')
		if next < 0 {
			break
		}
		start += next + 1
	}
	return "", false
}

func equalFoldASCII(b []byte, s string) bool {
	for i := range b {
		c := b[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != s[i] {
			return false
		}
	}
	return true
}
