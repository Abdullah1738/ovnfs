package refs

import (
	"errors"
	"strings"
	"testing"
)

var nastyRefNames = []string{
	"refs/heads/main", "main", "HEAD", "refs/tags/v1.0", "refs/heads/a.b", "refs/heads/ü",
	"refs/heads/a.lock", "refs/heads/a.lock/b", "refs/heads/a.lockx", "refs/heads/.lock", "refs/heads/a/b.lock",
	"refs/heads/.hidden", "refs/heads/a/.b", "x/./y", "refs/heads/..", "refs/heads/a..b", "refs/heads/a.", "foo.",
	"refs/heads/a@{b", "refs/heads/@{", "@", "refs/heads/@", "refs/heads/a@b", "@/x", "x/@",
	"refs/heads/a\\b", "refs/heads/a b", "refs/heads/a~b", "refs/heads/a^b", "refs/heads/a:b",
	"refs/heads/a?b", "refs/heads/a[b", "refs/heads/{a}", "refs/heads/a\x01", "refs/heads/a\x7f", "refs/heads/a\tb",
	"refs/heads/a*b", "refs/heads/a*b*", "refs/heads/*", "*", "refs/*/x", "*/x", "refs/heads/*.lock",
	"/refs/heads/a", "refs/heads/a/", "refs//heads/a", "//refs///heads//a", "refs/heads/", "///", "/",
	"", ".", "..", "a/..", "refs/heads/-a", "-a/b", "a//b", "/a", "a/b/", "a/b.", "a/.",
}

// TestCheckRefFormatMatchesGit runs git check-ref-format over nasty names
// with each option combination.
func TestCheckRefFormatMatchesGit(t *testing.T) {
	requireGit(t)
	dir, env := t.TempDir(), isolatedGitEnv(t)
	variants := []struct {
		flags []string
		opts  FormatOptions
	}{
		{nil, FormatOptions{}},
		{[]string{"--allow-onelevel"}, FormatOptions{AllowOneLevel: true}},
		{[]string{"--refspec-pattern"}, FormatOptions{RefspecPattern: true}},
		{[]string{"--normalize"}, FormatOptions{Normalize: true}},
		{[]string{"--normalize", "--allow-onelevel", "--refspec-pattern"}, FormatOptions{Normalize: true, AllowOneLevel: true, RefspecPattern: true}},
	}
	for _, v := range variants {
		for _, name := range nastyRefNames {
			if strings.HasPrefix(name, "-") {
				// git parses it as an option and has no "--"; the
				// documentation allows a leading dash in a ref component.
				if _, err := CheckRefFormat(name, v.opts); err != nil {
					t.Errorf("CheckRefFormat(%q, %+v) = %v, want accepted", name, v.opts, err)
				}
				continue
			}
			stdout, _, gitErr := runGitIn(dir, env, append(append([]string{"check-ref-format"}, v.flags...), name)...)
			got, err := CheckRefFormat(name, v.opts)
			switch {
			case (gitErr == nil) != (err == nil):
				t.Errorf("CheckRefFormat(%q, %+v) error %v; git accepts: %v", name, v.opts, err, gitErr == nil)
			case err != nil && !errors.Is(err, ErrInvalidName):
				t.Errorf("CheckRefFormat(%q) error %v does not wrap ErrInvalidName", name, err)
			case err == nil && v.opts.Normalize && got != stdout:
				t.Errorf("CheckRefFormat(%q, %+v) = %q; git prints %q", name, v.opts, got, stdout)
			case err == nil && !v.opts.Normalize && got != name:
				t.Errorf("CheckRefFormat(%q) = %q, want the name unchanged", name, got)
			}
		}
	}
}

// TestCheckBranchNameMatchesGit compares with git check-ref-format --branch,
// run outside any repository so that no "@{-N}" expansion can apply.
func TestCheckBranchNameMatchesGit(t *testing.T) {
	requireGit(t)
	dir, env := t.TempDir(), isolatedGitEnv(t)
	names := append([]string{
		"-main", "-", "x-", "a/-b", "heads/HEAD", "refs/heads/HEAD", "FETCH_HEAD", "head",
		"feature/x", "@{-1}", "@{1}", "a@{b",
	}, nastyRefNames...)
	for _, name := range names {
		_, _, gitErr := runGitIn(dir, env, "check-ref-format", "--branch", name)
		err := CheckBranchName(name)
		if (gitErr == nil) != (err == nil) {
			t.Errorf("CheckBranchName(%q) = %v; git accepts: %v", name, err, gitErr == nil)
		}
		if err != nil && !errors.Is(err, ErrInvalidName) {
			t.Errorf("CheckBranchName(%q) error %v does not wrap ErrInvalidName", name, err)
		}
	}
}
