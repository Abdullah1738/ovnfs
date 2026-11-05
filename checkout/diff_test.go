package checkout

import (
	"strings"
	"testing"
)

// TestUnifiedMatchesGNUDiff compares edge cases with `diff -u --label` output
// from GNU diffutils 3.12: missing final newlines, empty and absent sides,
// single-line ranges, and the gap at which neighboring hunks merge.
func TestUnifiedMatchesGNUDiff(t *testing.T) {
	for _, test := range []struct {
		name     string
		old, cur []byte
		want     string
	}{
		{"noeol change", []byte("a\nb\nc"), []byte("a\nb\nd"), "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n b\n-c\n\\ No newline at end of file\n+d\n\\ No newline at end of file\n"},
		{"eol added", []byte("a\nb"), []byte("a\nb\n"), "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n"},
		{"noeol context", []byte("x\na\nb"), []byte("y\na\nb"), "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n-x\n+y\n a\n b\n\\ No newline at end of file\n"},
		{"added", nil, []byte("1\n2\n"), "--- /dev/null\n+++ b/f\n@@ -0,0 +1,2 @@\n+1\n+2\n"},
		{"deleted", []byte("1\n"), nil, "--- a/f\n+++ /dev/null\n@@ -1 +0,0 @@\n-1\n"},
		{"from empty", []byte{}, []byte("1\n"), "--- a/f\n+++ b/f\n@@ -0,0 +1 @@\n+1\n"},
		{"equal", []byte("a\n"), []byte("a\n"), ""},
		{"gap 6 merges", []byte("x\n1\n2\n3\n4\n5\n6\ny\n"), []byte("X\n1\n2\n3\n4\n5\n6\nY\n"), "--- a/f\n+++ b/f\n@@ -1,8 +1,8 @@\n-x\n+X\n 1\n 2\n 3\n 4\n 5\n 6\n-y\n+Y\n"},
		{"gap 7 splits", []byte("x\n1\n2\n3\n4\n5\n6\n7\ny\n"), []byte("X\n1\n2\n3\n4\n5\n6\n7\nY\n"), "--- a/f\n+++ b/f\n@@ -1,4 +1,4 @@\n-x\n+X\n 1\n 2\n 3\n@@ -6,4 +6,4 @@\n 5\n 6\n 7\n-y\n+Y\n"},
		{"middle", []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n"), []byte("1\n2\n3\n4\n6\n7\n8\n9\n"), "--- a/f\n+++ b/f\n@@ -2,7 +2,6 @@\n 2\n 3\n 4\n-5\n 6\n 7\n 8\n"},
	} {
		var w strings.Builder
		unified(&w, "f", test.old, test.cur)
		if w.String() != test.want {
			t.Errorf("%s:\n%s\nwant:\n%s", test.name, w.String(), test.want)
		}
	}
}
