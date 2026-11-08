package refs

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidName reports a reference name that git-check-ref-format rejects.
var ErrInvalidName = errors.New("invalid reference name")

// FormatOptions selects the git check-ref-format variant to apply.
type FormatOptions struct {
	// AllowOneLevel accepts names without a '/' (--allow-onelevel).
	AllowOneLevel bool
	// RefspecPattern accepts a single '*' anywhere in the name (--refspec-pattern).
	RefspecPattern bool
	// Normalize strips leading slashes and collapses runs of slashes before
	// checking (--normalize).
	Normalize bool
}

// CheckRefFormat applies git check-ref-format's rules to name and returns the
// accepted name: name itself, or its normalized form when opts.Normalize is set.
// A rejected name yields an error wrapping ErrInvalidName.
func CheckRefFormat(name string, opts FormatOptions) (string, error) {
	if opts.Normalize {
		name = normalizeSlashes(name)
	}
	if err := checkRefFormat(name, opts); err != nil {
		return "", fmt.Errorf("%w %q: %s", ErrInvalidName, name, err)
	}
	return name, nil
}

// normalizeSlashes drops leading slashes and collapses runs of slashes.
func normalizeSlashes(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	prevSlash := true // drops leading slashes
	for i := range len(name) {
		c := name[i]
		if c == '/' && prevSlash {
			continue
		}
		prevSlash = c == '/'
		b.WriteByte(c)
	}
	return b.String()
}

func checkRefFormat(name string, opts FormatOptions) error {
	if name == "" {
		return errors.New("empty name")
	}
	if name == "@" {
		return errors.New("name is '@'")
	}
	if name[0] == '/' || name[len(name)-1] == '/' {
		return errors.New("begins or ends with '/'")
	}
	if name[len(name)-1] == '.' {
		return errors.New("ends with '.'")
	}
	if !opts.AllowOneLevel && !strings.Contains(name, "/") {
		return errors.New("has only one level")
	}
	stars := 0
	for i := range len(name) {
		switch c := name[i]; {
		case c < 0x20 || c == 0x7f:
			return errors.New("contains a control character")
		case c == ' ', c == '~', c == '^', c == ':', c == '?', c == '[', c == '\\':
			return fmt.Errorf("contains %q", c)
		case c == '*':
			stars++
			if !opts.RefspecPattern || stars > 1 {
				return errors.New("contains '*'")
			}
		case c == '.' && i+1 < len(name) && name[i+1] == '.':
			return errors.New("contains '..'")
		case c == '@' && i+1 < len(name) && name[i+1] == '{':
			return errors.New("contains '@{'")
		}
	}
	for component := range strings.SplitSeq(name, "/") {
		switch {
		case component == "":
			return errors.New("contains '//'")
		case component[0] == '.':
			return errors.New("a component begins with '.'")
		case strings.HasSuffix(component, ".lock"):
			return errors.New("a component ends with '.lock'")
		}
	}
	return nil
}

// CheckBranchName reports whether name may be used as a new branch name, as
// git check-ref-format --branch does: refs/heads/<name> must be a valid ref
// name, and name must neither start with '-' nor be "HEAD". Git expands
// "@{-N}" and "<branch>@{upstream}" first; that needs reflog and config
// access this package does not have, so such names are rejected.
func CheckBranchName(name string) error {
	switch {
	case strings.Contains(name, "@{"):
		return fmt.Errorf("%w %q: '@{...}' branch expansion is not supported", ErrInvalidName, name)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("%w %q: a branch name cannot start with '-'", ErrInvalidName, name)
	case name == "HEAD":
		return fmt.Errorf("%w %q: a branch cannot be named HEAD", ErrInvalidName, name)
	}
	if err := checkRefFormat("refs/heads/"+name, FormatOptions{}); err != nil {
		return fmt.Errorf("%w %q: %s", ErrInvalidName, name, err)
	}
	return nil
}
