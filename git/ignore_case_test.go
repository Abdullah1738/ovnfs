package git

import (
	"path/filepath"
	"testing"
)

// TestIgnoreCase honors the repository's case-folding configuration for rules and scopes.
func TestIgnoreCase(t *testing.T) {
	r := testRepo(t)
	testGit(t, r.Top, "config", "core.ignorecase", "true")
	testWrite(t, filepath.Join(r.Top, ".gitignore"), "*.LOG\n/RootOnly\n")
	testWrite(t, filepath.Join(r.Top, "Sub", ".gitignore"), "/LOCAL\n")
	matcher, err := testIgnore(r, r.Top)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"error.log", "ERROR.LOG", "rootonly", "ROOTONLY", "Sub/local", "Sub/LOCAL"} {
		testGit(t, r.Top, "check-ignore", "--no-index", "--", name)
		if !matcher.Ignored(name, false) {
			t.Errorf("case-folded path %q was not ignored", name)
		}
	}
}
