package glob

import (
	"errors"
	"fmt"
	"strings"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/pattern"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
)

// Selector is a path glob as every search tool reads it: [Glob] selects
// entries with it and grep restricts the files it reads with it, so one
// pattern means the same paths everywhere. Patterns use [pattern] syntax
// against '/'-separated walk-relative paths, after omp's normalisation
// (pi-walker's build_pattern): backslashes become '/', unclosed '{' groups
// are closed, and unless anchored a pattern without '/' that neither starts
// with "**" nor is an exact brace union (`{a,b}`) matches at any depth, as
// if prefixed with "**/".
type Selector struct {
	glob *pattern.Glob
	// prune is false when the glob only inspects base names, so every
	// directory may hold matches and MayMatchUnder need not be asked.
	prune       bool
	nodeModules bool
}

// buildPattern is omp's normalisation of a path glob (see [Selector]).
func buildPattern(glob string, anchored bool) string {
	out := strings.ReplaceAll(glob, `\`, "/")
	if !anchored && !strings.Contains(out, "/") && !strings.HasPrefix(out, "**") && !isExactBraceUnion(out) {
		out = "**/" + out
	}
	if open := strings.Count(out, "{") - strings.Count(out, "}"); open > 0 {
		out += strings.Repeat("}", open)
	}
	return out
}

// isExactBraceUnion reports whether glob is `{a,b}` whose alternatives hold
// no wildcard, class, or nested group.
func isExactBraceUnion(glob string) bool {
	if len(glob) < 2 || glob[0] != '{' || glob[len(glob)-1] != '}' {
		return false
	}
	inner := glob[1 : len(glob)-1]
	return inner != "" && !strings.ContainsAny(inner, "*?[]{}")
}

func newSelector(g *pattern.Glob, nodeModules bool) *Selector {
	return &Selector{glob: g, prune: !g.BasenameOnly(), nodeModules: nodeModules}
}

// NewSelector compiles glob, the entries the glob tool selects; anchored is
// the opposite of omp's recursive mode. Errors: an empty pattern, or a
// malformed one (wrapping [pattern.ErrInvalid]).
func NewSelector(glob string, anchored bool) (*Selector, error) {
	if glob == "" {
		return nil, errors.New("empty glob")
	}
	built := buildPattern(glob, anchored)
	g, err := pattern.Compile(built, pattern.Options{})
	if err != nil {
		return nil, fmt.Errorf("glob %q: %w", glob, err)
	}
	return newSelector(g, strings.Contains(built, "node_modules")), nil
}

// NewScope compiles glob as the files a content search reads (grep): files
// it matches plus every file under a directory it matches, so `{src,test}`,
// `sr?`, or `packages/*/src` search those directories; `dir/*.ts` stays
// limited to dir's direct children (and files under directories named
// `*.ts` there). Errors as [NewSelector].
func NewScope(glob string, anchored bool) (*Selector, error) {
	if glob == "" {
		return nil, errors.New("empty glob")
	}
	built := buildPattern(glob, anchored)
	g, err := pattern.Compile(built, pattern.Options{})
	if err != nil {
		return nil, fmt.Errorf("glob %q: %w", glob, err)
	}
	if base := strings.TrimRight(built, "/"); base != "**" && !strings.HasSuffix(base, "/**") {
		under, err := pattern.Compile(base+"/**", pattern.Options{})
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", glob, err)
		}
		g = pattern.Union(g, under)
	}
	return newSelector(g, strings.Contains(built, "node_modules")), nil
}

// NewExclude compiles the paths a content search leaves out (grep's
// exclude): a pattern without '/' matches at any depth (`*.test.ts`,
// `vendor`, `{vendor,gen}`), a leading '/' anchors it to the root
// (`/vendor`), backslashes become '/', surrounding whitespace and trailing
// slashes are ignored. Callers skip matched files and do not enter matched
// directories. It returns nil when no pattern is left after trimming, and
// errors on a malformed pattern (wrapping [pattern.ErrInvalid]).
func NewExclude(patterns []string) (*Selector, error) {
	var globs []*pattern.Glob
	for _, src := range patterns {
		p := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(src), `\`, "/"), "/")
		if p == "" {
			continue
		}
		var built string
		switch {
		case strings.HasPrefix(p, "/"):
			built = p[1:]
		case !strings.Contains(p, "/") && !strings.HasPrefix(p, "**"):
			built = buildPattern("**/"+p, false)
		default:
			built = buildPattern(p, false)
		}
		g, err := pattern.Compile(built, pattern.Options{})
		if err != nil {
			return nil, fmt.Errorf("exclude %q: %w", src, err)
		}
		globs = append(globs, g)
	}
	if len(globs) == 0 {
		return nil, nil
	}
	return newSelector(pattern.Union(globs...), false), nil
}

// Matches reports whether e's path matches.
func (s *Selector) Matches(e *walk.Entry) bool { return s.glob.MatchBase(e.Path, e.Name()) }

// MatchPath reports whether the walk-relative path matches.
func (s *Selector) MatchPath(path string) bool { return s.glob.Match(path) }

// Excludes reports whether nothing below the directory e can match, so a
// walk filter can return SkipDir for it.
func (s *Selector) Excludes(e *walk.Entry) bool {
	return e.Type == fsys.Dir && s.Prunes(e.Path)
}

// Prunes reports whether nothing strictly below directory dir can match.
func (s *Selector) Prunes(dir string) bool {
	return s.prune && !s.glob.MayMatchUnder(dir)
}

// Bound narrows wo to where matches can be: no deeper than the pattern
// reaches, and into node_modules whenever the pattern names it.
func (s *Selector) Bound(wo *walk.Options) {
	if d := s.glob.MaxDepth(); d > 0 && (wo.MaxDepth == 0 || d < wo.MaxDepth) {
		wo.MaxDepth = d
	}
	if s.nodeModules {
		wo.SkipNodeModules = false
	}
}
