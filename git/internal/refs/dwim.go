package refs

import (
	"errors"
	"io/fs"
	"strings"
)

// dwimRules are gitrevisions' <refname> rules, in priority order: a short
// name means the first of these that names an existing reference.
var dwimRules = [...]struct{ prefix, suffix string }{
	{"", ""},
	{"refs/", ""},
	{"refs/tags/", ""},
	{"refs/heads/", ""},
	{"refs/remotes/", ""},
	{"refs/remotes/", "/HEAD"},
}

// Expand resolves a short reference name by the gitrevisions <refname>
// rules (<name> itself — e.g. HEAD, FETCH_HEAD —, refs/<name>,
// refs/tags/<name>, refs/heads/<name>, refs/remotes/<name>,
// refs/remotes/<name>/HEAD). It returns the full name from the first rule
// naming an existing reference, and whether another rule also matched (git
// then warns that the name is ambiguous). Symbolic references are not
// followed in the result: Expand("HEAD") is "HEAD". No match yields an error
// wrapping os.ErrNotExist.
func (s *Store) Expand(short string) (full string, ambiguous bool, err error) {
	for _, rule := range dwimRules {
		candidate := rule.prefix + short + rule.suffix
		ok, err := s.exists(candidate)
		if err != nil {
			return "", false, err
		}
		if !ok {
			continue
		}
		if full != "" {
			return full, true, nil
		}
		full = candidate
	}
	if full == "" {
		return "", false, notExist(short)
	}
	return full, false, nil
}

// exists reports whether name resolves to an object, which is what git's
// rules mean by a reference existing: a dangling symbolic reference does
// not count, and neither does a name git would not accept.
func (s *Store) exists(name string) (bool, error) {
	_, err := s.Resolve(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, ErrInvalidName),
		errors.Is(err, ErrSymrefDepth), errors.Is(err, errBroken):
		return false, nil
	}
	return false, err
}

// ShortenOptions configures Shorten.
type ShortenOptions struct {
	// Loose only avoids names that an earlier rule would resolve to another
	// reference (--abbrev-ref=loose, core.warnAmbiguousRefs=false); the
	// default strict mode avoids names any other rule would match.
	Loose bool
}

// Shorten returns the shortest name that the gitrevisions rules expand back
// to full without ambiguity, as git rev-parse --abbrev-ref prints it:
// refs/heads/x → "x", refs/remotes/origin/HEAD → "origin", but "heads/x"
// when refs/tags/x (or, in strict mode, refs/remotes/x) also exists. A name
// no shorter form reaches unambiguously is returned unchanged.
func (s *Store) Shorten(full string, opts ShortenOptions) (string, error) {
	for i := len(dwimRules) - 1; i > 0; i-- {
		rule := dwimRules[i]
		short, ok := strings.CutPrefix(full, rule.prefix)
		if !ok {
			continue
		}
		if short, ok = strings.CutSuffix(short, rule.suffix); !ok || short == "" {
			continue
		}
		ambiguous := false
		for j, other := range dwimRules {
			if j == i || opts.Loose && j > i {
				continue
			}
			found, err := s.exists(other.prefix + short + other.suffix)
			if err != nil {
				return "", err
			}
			if found {
				ambiguous = true
				break
			}
		}
		if !ambiguous {
			return short, nil
		}
	}
	return full, nil
}
