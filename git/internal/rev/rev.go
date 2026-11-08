// Package rev resolves git revision expressions (gitrevisions) to object
// names, maps branches to their upstream and push destinations, and answers
// commit-graph questions (ancestry, merge bases).
//
// Resolution follows `git rev-parse --verify`: an expression must name
// exactly one object. Commits are read through the Objects a caller supplies,
// so replace refs apply when the caller's reader applies them, and the
// caller's shallow set hides the parents of shallow commits.
//
// Written from git's documentation (gitrevisions, git-rev-parse,
// git-merge-base, gitformat-commit-graph, technical/commit-graph,
// technical/paint-down-to-common, git-config) and from observing git.
package rev

import (
	"errors"
	"fmt"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// Objects is the object access rev needs. *object.DB satisfies it; a
// repository passes a reader that applies replace refs.
type Objects interface {
	Format() object.Format
	Read(id object.ID) (object.Kind, []byte, error)
	Header(id object.ID) (object.Kind, int64, error)
	Has(id object.ID) bool
	FindPrefix(hexPrefix string, limit int) ([]object.ID, error)
	// Dirs lists the object directories (own first, then alternates),
	// where commit-graph files live.
	Dirs() []string
}

// Source is the repository state revisions are resolved against.
type Source struct {
	Refs    *refs.Store
	Objects Objects
	// Config is the repository's configuration (branch.*, remote.*, push.*,
	// core.disambiguate, core.commitGraph, core.warnAmbiguousRefs); nil
	// means none.
	Config *config.Config
	// Shallow holds the commits whose parents are hidden (<common dir>/shallow).
	Shallow map[object.ID]bool
	// Replaced reports that Objects applies at least one replace ref. The
	// commit-graph files then describe a different history, so, like git,
	// rev does not use them.
	Replaced bool
	// Index reads the index for :<n>:<path>; nil means there is none.
	Index func() (*index.Index, error)
}

// Options adjusts Resolve.
type Options struct {
	// EndOfOptions treats a leading '-' as part of the revision, as after
	// `git rev-parse --end-of-options`; otherwise such a spec is rejected
	// like the unknown option git would take it for.
	EndOfOptions bool
	// Prefix is the current directory relative to the top of the work tree
	// ("" at the top, else slash-separated with no trailing slash); paths
	// starting with ./ or ../ in <rev>:<path> and :<path> are relative to it.
	Prefix string
}

var (
	// ErrUnknown reports a revision that names nothing.
	ErrUnknown = errors.New("unknown revision or path not in the working tree")
	// ErrAmbiguous reports a short object name matching several objects.
	ErrAmbiguous = errors.New("ambiguous object name")
	// ErrNoUpstream reports a branch without (usable) upstream configuration.
	ErrNoUpstream = errors.New("no upstream configured")
	// ErrNotBranch reports a branch-only suffix (@{upstream}, @{push}) used
	// while HEAD is detached or on a name that is not a branch.
	ErrNotBranch = errors.New("not a branch")
	// ErrPeel reports an object that cannot be peeled to the requested type.
	ErrPeel = errors.New("object cannot be peeled to the requested type")
	// ErrUnsupported reports syntax that names more than one object (ranges,
	// ^@, ^!, ^-) or that is reserved (":/!x").
	ErrUnsupported = errors.New("revision syntax names no single object")
)

// Error is a failure to resolve one revision expression.
type Error struct {
	Spec string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("revision %q: %v", e.Spec, e.Err) }

// Unwrap returns the cause, which wraps one of the Err* sentinels.
func (e *Error) Unwrap() error { return e.Err }

// Resolve returns the object spec names, with `git rev-parse --verify`
// semantics: exactly one object, else an *Error.
func Resolve(src *Source, spec string, opts Options) (object.ID, error) {
	if len(spec) > 0 && spec[0] == '-' && !opts.EndOfOptions {
		return object.ID{}, &Error{Spec: spec, Err: fmt.Errorf("%w: looks like an option", ErrUnknown)}
	}
	r := newResolver(src, opts)
	id, err := r.resolve(spec, hintNone)
	if err != nil {
		return object.ID{}, &Error{Spec: spec, Err: err}
	}
	return id, nil
}
