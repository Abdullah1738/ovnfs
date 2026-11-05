package grep

import (
	"fmt"
	"strings"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/glob"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
)

// fileFilter selects the files a search reads: Options.Glob (as a scope)
// and Options.Exclude against the walk-relative path, Options.Type against
// the base name. It is the single selection stage of both search
// strategies (walk visitor and sorted collection), so they can never
// disagree about candidates.
type fileFilter struct {
	sel *glob.Selector // nil: every path
	ex  *glob.Selector // nil: nothing excluded
	typ *fileType      // nil: every type
}

// newFileFilter compiles opts.Glob as a scope ([glob.NewScope]: the files
// it matches and every file under a directory it matches) and opts.Exclude
// ([glob.NewExclude]), and resolves opts.Type.
func newFileFilter(opts *Options) (*fileFilter, error) {
	f := &fileFilter{typ: lookupType(opts.Type)}
	if src := strings.TrimSpace(opts.Glob); src != "" {
		sel, err := glob.NewScope(src, opts.Anchored)
		if err != nil {
			return nil, fmt.Errorf("grep: %w", err)
		}
		f.sel = sel
	}
	ex, err := glob.NewExclude(opts.Exclude)
	if err != nil {
		return nil, fmt.Errorf("grep: %w", err)
	}
	f.ex = ex
	return f, nil
}

// visit decides a walk entry: Continue for a directory to descend or a file
// to search, Skip for files to ignore, SkipDir for excluded subtrees and
// subtrees no match can be in. Callers search only files that get Continue.
func (f *fileFilter) visit(e *walk.Entry) walk.Action {
	switch e.Type {
	case fsys.Dir:
		if (f.ex != nil && f.ex.Matches(e)) || (f.sel != nil && f.sel.Excludes(e)) {
			return walk.SkipDir
		}
		return walk.Skip
	case fsys.File:
		if f.typ.matches(e.Name()) && (f.sel == nil || f.sel.Matches(e)) && (f.ex == nil || !f.ex.Matches(e)) {
			return walk.Continue
		}
	}
	// Symlinks are never followed or searched.
	return walk.Skip
}

// tighten narrows the walk to where the glob can match (see
// [glob.Selector.Bound]).
func (f *fileFilter) tighten(wo *walk.Options) {
	if f.sel != nil {
		f.sel.Bound(wo)
	}
}

// fileType is one entry of the type table: file name extensions and whole
// base names, both compared ASCII case-insensitively.
type fileType struct {
	exts  []string
	names []string
}

// fileTypes is the grep type table: each
// alias maps to the extensions and file names it selects. A type that is
// not listed selects files with that extension or that exact name.
var fileTypes = func() map[string]*fileType {
	m := map[string]*fileType{}
	add := func(aliases string, exts string, names string) {
		t := &fileType{exts: strings.Fields(exts), names: strings.Fields(names)}
		for a := range strings.FieldsSeq(aliases) {
			m[a] = t
		}
	}
	add("js javascript", "js jsx mjs cjs", "")
	add("ts typescript", "ts tsx mts cts", "")
	add("json", "json jsonc json5", "")
	add("yaml yml", "yaml yml", "")
	add("toml", "toml", "")
	add("md markdown", "md markdown mdx", "")
	add("py python", "py pyi", "")
	add("rs rust", "rs", "")
	add("go", "go", "")
	add("java", "java", "")
	add("kt kotlin", "kt kts", "")
	add("c", "c h", "")
	add("cpp cxx", "cpp cc cxx hpp hxx hh", "")
	add("cs csharp", "cs csx", "")
	add("php", "php phtml", "")
	add("rb ruby", "rb rake gemspec", "")
	add("sh bash", "sh bash zsh", "")
	add("zsh", "zsh", "")
	add("fish", "fish", "")
	add("html", "html htm", "")
	add("css", "css", "")
	add("scss", "scss", "")
	add("sass", "sass", "")
	add("less", "less", "")
	add("xml", "xml", "")
	add("docker dockerfile", "", "dockerfile")
	add("make makefile", "", "makefile")
	return m
}()

// lookupType resolves Options.Type: trimmed, a leading '.' dropped, case
// ignored; nil for an empty type.
func lookupType(name string) *fileType {
	name = strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "."))
	if name == "" {
		return nil
	}
	if t, ok := fileTypes[name]; ok {
		return t
	}
	return &fileType{exts: []string{name}, names: []string{name}}
}

// matches reports whether base name belongs to t: an exact name, or an
// extension after the last '.' (so ".bashrc" has extension "bashrc"). A nil
// t matches everything.
func (t *fileType) matches(name string) bool {
	if t == nil {
		return true
	}
	for _, n := range t.names {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return false
	}
	ext := name[i+1:]
	for _, e := range t.exts {
		if strings.EqualFold(ext, e) {
			return true
		}
	}
	return false
}
