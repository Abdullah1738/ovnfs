// Package refs reads and updates a repository's references — branches, tags,
// HEAD and other pseudorefs — and their reflogs, in either of git's ref
// storage formats: the "files" backend (loose ref files, packed-refs, logs/)
// and the "reftable" backend (a stack of binary tables under reftable/).
//
// It also implements git's reference name rules (git check-ref-format) and
// short-name resolution (the <refname> rules of gitrevisions and the
// abbreviation of git rev-parse --abbrev-ref).
//
// The package never reads objects: peeling tags is the caller's business.
// Everything here is written from git's documentation (gitrepository-layout,
// technical/reftable, git-update-ref, git-check-ref-format, gitrevisions) and
// from observing the git binary.
package refs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

var (
	// ErrLocked reports that a lock file git uses to exclude concurrent
	// writers already exists: another git (or ovnfs) process is updating the
	// same reference, or a crashed one left the lock behind.
	ErrLocked = errors.New("reference lock is held")
	// ErrMismatch reports a compare-and-swap failure: the reference's current
	// value is not the expected old value.
	ErrMismatch = errors.New("reference does not have the expected value")
	// ErrNotSymbolic reports that a reference holds an object name rather
	// than naming another reference (a detached HEAD).
	ErrNotSymbolic = errors.New("not a symbolic reference")
	// ErrSymrefDepth reports a chain of symbolic references longer than git
	// follows.
	ErrSymrefDepth = errors.New("symbolic reference chain too deep")
)

// maxRefReads is how many references git reads while resolving a name before
// giving up: a ref may go through at most four symbolic hops (observed with
// git rev-parse and git symbolic-ref).
const maxRefReads = 5

// Backend names a ref storage format, the value of extensions.refStorage.
type Backend string

const (
	// BackendFiles stores refs as loose files plus packed-refs; the default.
	BackendFiles Backend = "files"
	// BackendReftable stores refs in a stack of reftable files.
	BackendReftable Backend = "reftable"
)

// Options locates a repository's references.
type Options struct {
	// GitDir is the repository (or linked worktree) git directory; it holds
	// the per-worktree refs.
	GitDir string
	// CommonDir holds the shared refs; "" means GitDir.
	CommonDir string
	// Format is the repository's object hash format.
	Format object.Format
	// Backend is the ref storage format; "" means BackendFiles.
	Backend Backend
}

// Store reads and writes one repository's references. It keeps no state
// between calls, so it is safe for concurrent use and always sees changes
// made by other processes.
type Store struct {
	gitDir, commonDir string
	format            object.Format
	backend           backend
}

// Ref is a reference as stored: either direct (ID set) or symbolic (Target set).
type Ref struct {
	// Name is the full reference name, e.g. "refs/heads/main" or "HEAD".
	Name string
	// ID is the object a direct reference points at; zero for a symbolic one.
	ID object.ID
	// Target is the reference a symbolic reference points at.
	Target string
	// Peeled is the non-tag object an annotated tag reference ultimately
	// points at, when the store records it (packed-refs '^' lines, reftable
	// two-object records); zero when not recorded or not a tag.
	Peeled object.ID
}

// Symbolic reports whether r names another reference.
func (r Ref) Symbolic() bool { return r.Target != "" }

// ReflogEntry is one recorded update of a reference.
type ReflogEntry struct {
	// Old and New are the values before and after; Old is the zero name
	// of the repository's format for a creation.
	Old, New object.ID
	// Committer is who made the update, and when.
	Committer object.Signature
	// Message is the reason given, "" if none.
	Message string
}

// backend is one storage format. Names it receives are already routed: loc
// says which git directory (and, for the files backend, whether packed-refs
// applies) and name is the ref name within it.
type backend interface {
	read(loc location, name string) (Ref, error)
	// list returns the refs under refs/ starting with prefix stored at loc,
	// sorted by name, keeping only names for which keep returns true.
	list(loc location, prefix string, keep func(string) bool) ([]Ref, error)
	reflog(loc location, name string) ([]ReflogEntry, error)
	commit(s *Store, tx *transaction) error
}

// Open returns a Store for the repository described by opts.
func Open(opts Options) (*Store, error) {
	if opts.GitDir == "" {
		return nil, errors.New("refs: GitDir is required")
	}
	if opts.Format != object.SHA1 && opts.Format != object.SHA256 {
		return nil, fmt.Errorf("refs: unsupported object format %d", opts.Format)
	}
	s := &Store{gitDir: opts.GitDir, commonDir: opts.CommonDir, format: opts.Format}
	if s.commonDir == "" {
		s.commonDir = s.gitDir
	}
	switch opts.Backend {
	case "", BackendFiles:
		s.backend = filesBackend{}
	case BackendReftable:
		s.backend = reftableBackend{}
	default:
		return nil, fmt.Errorf("refs: unsupported ref storage format %q", opts.Backend)
	}
	return s, nil
}

// Read returns the reference called name without following symbolic
// references. A missing reference yields an error wrapping os.ErrNotExist;
// a name git check-ref-format --allow-onelevel rejects, ErrInvalidName.
func (s *Store) Read(name string) (Ref, error) {
	loc, inner, err := s.route(name)
	if err != nil {
		return Ref{}, err
	}
	r, err := s.readAt(loc, inner)
	if err != nil {
		return Ref{}, err
	}
	r.Name = name
	if r.Symbolic() && loc.foreign {
		// A foreign worktree's HEAD names refs relative to that worktree;
		// shared refs are the same everywhere, so only per-worktree targets
		// need the prefix to be reachable from here.
		r.Target = loc.qualify(r.Target)
	}
	return r, nil
}

// readAt reads a routed name, handling the pseudorefs that both backends
// keep as plain files because they can hold more than one object name.
func (s *Store) readAt(loc location, name string) (Ref, error) {
	if isSpecialPseudoref(name) {
		return readLoose(loc.dir, name, s.format)
	}
	return s.backend.read(loc, name)
}

// Resolve follows symbolic references from name until a direct reference,
// returning it with Name set to the full name it ended at. A missing
// reference anywhere along the chain yields an error wrapping os.ErrNotExist.
func (s *Store) Resolve(name string) (Ref, error) {
	r, err := s.follow(name)
	if err != nil {
		return Ref{}, err
	}
	if r.Symbolic() {
		return Ref{}, notExist(r.Target)
	}
	return r, nil
}

// follow reads name and the references it points at until it reaches a
// direct reference or a missing one. For a dangling chain it returns the last
// symbolic reference read, whose Target does not exist.
func (s *Store) follow(name string) (Ref, error) {
	var last Ref
	for range maxRefReads {
		r, err := s.Read(name)
		if errors.Is(err, fs.ErrNotExist) && last.Symbolic() {
			return last, nil
		}
		if err != nil {
			return Ref{}, err
		}
		if !r.Symbolic() {
			return r, nil
		}
		last, name = r, r.Target
	}
	return Ref{}, fmt.Errorf("refs: %s: %w", name, ErrSymrefDepth)
}

// Symbolic returns the reference that the symbolic reference name points at,
// following further symbolic references as git symbolic-ref does by default;
// the result may name a reference that does not exist yet (an unborn
// branch). A direct reference (a detached HEAD) yields ErrNotSymbolic.
func (s *Store) Symbolic(name string) (string, error) {
	r, err := s.Read(name)
	if err != nil {
		return "", err
	}
	if !r.Symbolic() {
		return "", fmt.Errorf("refs: %s: %w", name, ErrNotSymbolic)
	}
	end, err := s.follow(name)
	if err != nil {
		return "", err
	}
	if end.Symbolic() {
		return end.Target, nil
	}
	return end.Name, nil
}

// List returns every reference whose full name starts with prefix, sorted by
// name, as git for-each-ref sees them from this worktree: shared refs plus
// this worktree's refs/bisect/, refs/worktree/ and refs/rewritten/. Only
// names under refs/ are listed. Values are as stored: symbolic references
// carry their Target, not the object it resolves to.
func (s *Store) List(prefix string) ([]Ref, error) {
	common := location{dir: s.commonDir, common: true, format: s.format}
	if s.gitDir == s.commonDir {
		return s.backend.list(common, prefix, func(string) bool { return true })
	}
	shared, err := s.backend.list(common, prefix, func(name string) bool { return !isPerWorktree(name) })
	if err != nil {
		return nil, err
	}
	own, err := s.backend.list(location{dir: s.gitDir, format: s.format}, prefix, isPerWorktree)
	if err != nil {
		return nil, err
	}
	return mergeSorted(shared, own), nil
}

// Reflog returns the recorded updates of the reference called name, newest
// first. Symbolic references are not followed: HEAD's reflog is HEAD's own.
// A reference without a reflog yields an error wrapping os.ErrNotExist.
func (s *Store) Reflog(name string) ([]ReflogEntry, error) {
	loc, inner, err := s.route(name)
	if err != nil {
		return nil, err
	}
	return s.backend.reflog(loc, inner)
}

// location is where a routed reference lives.
type location struct {
	// dir is the git directory holding the ref: its loose file and logs/ for
	// the files backend, its reftable/ stack for the reftable backend.
	dir string
	// common marks the shared ref namespace, where packed-refs applies.
	common bool
	format object.Format
	// foreign marks another worktree's per-worktree refs, reached through
	// a main-worktree/ or worktrees/<id>/ prefix.
	foreign bool
	prefix  string // the main-worktree/ or worktrees/<id>/ prefix
}

// qualify turns a ref name seen inside a foreign worktree into the name that
// reaches it from here.
func (l location) qualify(name string) string {
	if isPerWorktree(name) {
		return l.prefix + name
	}
	return name
}

// isPerWorktree reports whether name belongs to a single worktree: every
// name outside refs/ (HEAD and the other pseudorefs) plus refs/bisect/,
// refs/worktree/ and refs/rewritten/ (gitrepository-layout, git-worktree).
func isPerWorktree(name string) bool {
	if !strings.HasPrefix(name, "refs/") {
		return true
	}
	for _, p := range []string{"refs/bisect/", "refs/worktree/", "refs/rewritten/"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// isSpecialPseudoref reports the pseudorefs git keeps as files in every
// backend and refuses to update through the ref store.
func isSpecialPseudoref(name string) bool { return name == "FETCH_HEAD" || name == "MERGE_HEAD" }

// route validates name and maps it to the location holding it and its name
// there, resolving the main-worktree/ and worktrees/<id>/ prefixes that reach
// other worktrees' per-worktree refs.
func (s *Store) route(name string) (location, string, error) {
	loc, inner, err := s.routeDir(name)
	loc.format = s.format
	return loc, inner, err
}

func (s *Store) routeDir(name string) (location, string, error) {
	if _, err := CheckRefFormat(name, FormatOptions{AllowOneLevel: true}); err != nil {
		return location{}, "", err
	}
	if rest, ok := strings.CutPrefix(name, "main-worktree/"); ok {
		if !isPerWorktree(rest) {
			return location{}, "", notExist(name)
		}
		return location{dir: s.commonDir, foreign: true, prefix: "main-worktree/"}, rest, nil
	}
	if rest, ok := strings.CutPrefix(name, "worktrees/"); ok {
		id, inner, ok := strings.Cut(rest, "/")
		if !ok || !isPerWorktree(inner) {
			return location{}, "", notExist(name)
		}
		return location{
			dir:     filepath.Join(s.commonDir, "worktrees", id),
			foreign: true,
			prefix:  "worktrees/" + id + "/",
		}, inner, nil
	}
	if isPerWorktree(name) {
		return location{dir: s.gitDir}, name, nil
	}
	return location{dir: s.commonDir, common: true}, name, nil
}

// mergeSorted merges two name-sorted ref lists with disjoint names.
func mergeSorted(a, b []Ref) []Ref {
	out := make([]Ref, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		if a[0].Name < b[0].Name {
			out, a = append(out, a[0]), a[1:]
		} else {
			out, b = append(out, b[0]), b[1:]
		}
	}
	return append(append(out, a...), b...)
}

// notExist wraps os.ErrNotExist with the missing reference's name.
func notExist(name string) error { return fmt.Errorf("refs: %s: %w", name, os.ErrNotExist) }
