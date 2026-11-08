package git

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestObjectsMissingAndKinds reports a missing object as os.ErrNotExist (the
// capture walk hashes such files instead of failing) and refuses non-blobs as blobs.
func TestObjectsMissingAndKinds(t *testing.T) {
	r := testRepo(t)
	blob := testGit(t, r.Top, "rev-parse", "HEAD:tracked")
	objects, err := r.Objects()
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	if size, err := objects.Size(blob); err != nil || size != int64(len("original\n")) {
		t.Fatalf("Size = %d, %v", size, err)
	}
	if data, err := objects.Blob(blob); err != nil || string(data) != "original\n" {
		t.Fatalf("Blob = %q, %v", data, err)
	}
	missing := strings.Repeat("0", len(blob))
	if _, err := objects.Size(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Size of a missing object = %v", err)
	}
	if _, err := objects.Blob(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Blob of a missing object = %v", err)
	}
	if _, err := objects.Blob(testGit(t, r.Top, "rev-parse", "HEAD")); err == nil {
		t.Fatal("a commit was read as a blob")
	}
}
