package rev

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// SymbolicFullName returns the full ref name spec denotes, as
// `git rev-parse --symbolic-full-name`: symbolic refs are followed
// (HEAD → refs/heads/main, origin/HEAD → refs/remotes/origin/main), a
// detached HEAD is "HEAD", and @{upstream}, @{push} and @{-N} yield the refs
// they name. A spec naming an object but no ref (an object name, HEAD~1,
// main@{1}) yields "" and no error; one naming nothing is an *Error.
func SymbolicFullName(src *Source, spec string) (string, error) {
	r := newResolver(src, Options{EndOfOptions: true})
	full, err := r.symbolicFullName(spec)
	if err != nil {
		return "", &Error{Spec: spec, Err: err}
	}
	return full, nil
}

// AbbrevRef returns the shortest unambiguous name of the ref spec denotes,
// as `git rev-parse --abbrev-ref` ("main", "origin/main", "heads/x" when a
// tag x exists); "" for a spec naming no ref, "HEAD" for a detached HEAD.
// core.warnAmbiguousRefs=false selects git's loose abbreviation.
func AbbrevRef(src *Source, spec string) (string, error) {
	full, err := SymbolicFullName(src, spec)
	if err != nil || full == "" || full == "HEAD" {
		return full, err
	}
	loose := false
	if src.Config != nil {
		if strict, found, err := src.Config.Bool("core.warnambiguousrefs"); err == nil && found {
			loose = !strict
		}
	}
	return src.Refs.Shorten(full, refs.ShortenOptions{Loose: loose})
}

func (r *resolver) symbolicFullName(spec string) (string, error) {
	if full, ok, err := r.refOnly(spec); err != nil || ok {
		return full, err
	}
	if _, err := r.resolve(spec, hintNone); err != nil {
		return "", err
	}
	return "", nil
}

// refOnly evaluates a spec that is just a ref (with ref-producing @{...}
// suffixes), returning the fully dereferenced ref name; ok is false when
// spec is some other kind of revision.
func (r *resolver) refOnly(spec string) (full string, ok bool, err error) {
	if pathColon(spec) >= 0 || strings.HasPrefix(spec, ":") || strings.ContainsAny(spec, "^~") {
		return "", false, nil
	}
	switch {
	case spec == "@":
		full = "HEAD"
	case strings.HasSuffix(spec, "}") && strings.Contains(spec, "@{"):
		at := strings.LastIndex(spec, "@{")
		inner := spec[at+2 : len(spec)-1]
		isPrev := at == 0 && strings.HasPrefix(inner, "-")
		if !isPrev && branchSuffix(inner) == 0 {
			return "", false, nil // a reflog entry is an object, not a ref
		}
		if isPrev {
			prev, err := r.previousCheckout(atoiOr(inner[1:]))
			if err != nil {
				return "", false, err
			}
			// The name checked out is taken like one typed by the user
			// (a detached checkout recorded an object name).
			return r.refOnly(prev)
		}
		if full, err = r.refName(spec); err != nil {
			return "", false, err
		}
	default:
		if strings.Contains(spec, "@{") || spec == "" {
			return "", false, nil
		}
		var ambiguous bool
		full, ambiguous, err = r.src.Refs.Expand(spec)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, refs.ErrInvalidName):
			return "", false, nil
		case err != nil:
			return "", false, err
		case ambiguous:
			return "", false, nil // git names no ref for an ambiguous name
		}
	}
	ref, err := r.src.Refs.Resolve(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, unknown("%s does not exist", full)
		}
		return "", false, err
	}
	return ref.Name, true, nil
}

func atoiOr(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 0
		}
	}
	return n
}
