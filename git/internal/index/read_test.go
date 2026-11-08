package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// populate commits a small work tree and returns the repository.
func populate(t *testing.T, f object.Format) *testRepo {
	t.Helper()
	r := newTestRepo(t, f)
	for _, p := range []string{"a/x", "a/y/z", "a-b", "a.c", "b/1", "b/2", "c/d/e/f", "top"} {
		r.writeFile(p, "content "+p+"\n")
	}
	r.git("add", ".")
	r.git("commit", "-q", "-m", "init")
	return r
}

// checkListing compares a Read of name with git's view of the same file.
func checkListing(t *testing.T, r *testRepo, name string, opts ReadOptions, lsArgs ...string) *Index {
	t.Helper()
	idx := mustRead(t, name, r.format, opts)
	want := r.withIndex(name, append([]string{"ls-files", "--stage", "-z"}, lsArgs...)...)
	if got := lsStage(idx.Entries); got != want {
		t.Fatalf("entries:\n got %q\nwant %q", got, want)
	}
	if got, want := lsDebug(idx.Entries), r.gitDebug(name, lsArgs...); got != want {
		t.Fatalf("stat data:\n got %s\nwant %s", got, want)
	}
	return idx
}

func indexVersion(t *testing.T, name string) uint32 {
	t.Helper()
	return binary.BigEndian.Uint32(readFileBytes(t, name)[4:])
}

func TestReadMatchesGit(t *testing.T) {
	forEachFormat(t, func(t *testing.T, f object.Format) {
		t.Run("v2", func(t *testing.T) {
			r := populate(t, f)
			name := r.path(".git/index")
			idx := checkListing(t, r, name, ReadOptions{})
			if idx.Version != 2 || idx.CacheTree == nil || !idx.CacheTree.Valid() {
				t.Fatalf("version %d, cache tree %+v", idx.Version, idx.CacheTree)
			}
			info, _ := os.Stat(name)
			if !idx.ModTime.Equal(info.ModTime()) {
				t.Fatalf("ModTime %v, file mtime %v", idx.ModTime, info.ModTime())
			}
		})

		t.Run("v3 flags", func(t *testing.T) {
			r := populate(t, f)
			r.writeFile("new", "new\n")
			r.git("add", "-N", "new")
			r.git("update-index", "--skip-worktree", "b/1")
			r.git("update-index", "--assume-unchanged", "a.c")
			name := r.path(".git/index")
			if v := indexVersion(t, name); v != 3 {
				t.Fatalf("git wrote version %d", v)
			}
			idx := checkListing(t, r, name, ReadOptions{})
			for _, e := range idx.Entries {
				if e.IntentToAdd != (e.Path == "new") || e.SkipWorktree != (e.Path == "b/1") || e.AssumeValid != (e.Path == "a.c") {
					t.Errorf("%s: ita %v skip %v assume %v", e.Path, e.IntentToAdd, e.SkipWorktree, e.AssumeValid)
				}
			}
		})

		t.Run("v4", func(t *testing.T) {
			r := populate(t, f)
			r.git("update-index", "--index-version", "4")
			idx := checkListing(t, r, r.path(".git/index"), ReadOptions{})
			if idx.Version != 4 {
				t.Fatalf("version %d", idx.Version)
			}
		})

		for _, version := range []string{"2", "4"} {
			t.Run("split v"+version, func(t *testing.T) {
				r := populate(t, f)
				// A version change forces an unsplit write, so split afterwards.
				r.git("update-index", "--index-version", version)
				r.git("update-index", "--split-index")
				r.writeFile("a/x", "changed\n")
				r.writeFile("a/new", "new\n")
				r.writeFile("zz", "new\n")
				r.git("update-index", "--add", "a/x", "a/new", "zz")
				r.git("rm", "-q", "--cached", "b/2", "c/d/e/f")
				name := r.path(".git/index")
				if !bytes.Contains(readFileBytes(t, name), []byte("link")) {
					t.Fatal("git did not write a split index")
				}
				if shared, _ := filepath.Glob(r.path(".git/sharedindex.*")); len(shared) == 0 {
					t.Fatal("no shared index")
				}
				checkListing(t, r, name, ReadOptions{})
			})
		}

		t.Run("split with long bitmaps", func(t *testing.T) {
			// Enough entries that the EWAH bitmaps hold runs, not just literals.
			r := newTestRepo(t, f)
			for i := range 300 {
				r.writeFile(fmt.Sprintf("f%03d", i), fmt.Sprint(i))
			}
			r.git("add", ".")
			r.git("update-index", "--split-index")
			var gone []string
			for i := 200; i < 230; i++ {
				gone = append(gone, fmt.Sprintf("f%03d", i))
			}
			r.git(append([]string{"rm", "-q", "--cached"}, gone...)...)
			r.writeFile("f280", "changed")
			r.git("update-index", "f280")
			if !bytes.Contains(readFileBytes(t, r.path(".git/index")), []byte("link")) {
				t.Fatal("git did not write a split index")
			}
			checkListing(t, r, r.path(".git/index"), ReadOptions{})
		})

		t.Run("sparse", func(t *testing.T) {
			r := populate(t, f)
			r.git("sparse-checkout", "set", "--cone", "a")
			r.git("config", "index.sparse", "true")
			r.git("sparse-checkout", "reapply")
			name := r.path(".git/index")

			sparse := checkListing(t, r, name, ReadOptions{}, "--sparse")
			if !sparse.Sparse() {
				t.Fatal("no sparse-directory entries")
			}
			full := checkListing(t, r, name, ReadOptions{ExpandSparse: r.readTree})
			if full.Sparse() {
				t.Fatal("sparse entries left after expansion")
			}
			for _, e := range full.Entries {
				outside := !strings.HasPrefix(e.Path, "a/") && e.Path != "a-b" && e.Path != "a.c" && e.Path != "top"
				if e.SkipWorktree != outside {
					t.Errorf("%s: skip-worktree %v", e.Path, e.SkipWorktree)
				}
			}

			// git writes the same full index when told not to keep it sparse.
			want := filepath.Join(t.TempDir(), "full")
			copyFile(t, name, want)
			r.run([]string{"GIT_INDEX_FILE=" + want}, "", "-c", "index.sparse=false", "update-index", "--index-version", "3")
			got := filepath.Join(t.TempDir(), "ours")
			if err := WriteFile(got, full, WriteOptions{Worktree: r.dir}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFileBytes(t, got), readFileBytes(t, want)) {
				t.Fatalf("expanded index differs from git's:\n%s\n%s", r.gitDebug(got), r.gitDebug(want))
			}
		})

		t.Run("untracked cache, fsmonitor, offset table", func(t *testing.T) {
			r := populate(t, f)
			r.writeFile("u1", "")
			r.writeFile("a/u2", "")
			r.writeFile("a/.gitignore", "*.o\n")
			hook := filepath.Join(t.TempDir(), "fsmonitor")
			if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'tok123\\0'\nprintf 'top\\0'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := []string{"-c", "core.untrackedCache=true", "-c", "core.fsmonitor=" + hook}
			r.git(append(cfg, "status", "--porcelain")...)
			r.git(append(cfg, "status", "--porcelain")...)
			name := r.path(".git/index")
			idx := checkListing(t, r, name, ReadOptions{})
			if idx.Untracked == nil || idx.Untracked.Root == nil || idx.Untracked.ExcludePerDir != ".gitignore" {
				t.Fatalf("untracked cache %+v", idx.Untracked)
			}
			if !slices.Contains(idx.Untracked.Root.Untracked, "u1") {
				t.Errorf("root untracked %q", idx.Untracked.Root.Untracked)
			}
			if idx.FSMonitor == nil || idx.FSMonitor.Version != 2 || idx.FSMonitor.Token != "tok123" {
				t.Fatalf("fsmonitor %+v", idx.FSMonitor)
			}

			r.git("-c", "index.threads=4", "-c", "index.recordEndOfIndexEntries=true", "-c", "index.recordOffsetTable=true",
				"update-index", "--index-version", "2")
			data := readFileBytes(t, name)
			if !bytes.Contains(data, []byte("EOIE")) || !bytes.Contains(data, []byte("IEOT")) {
				t.Fatal("git wrote no EOIE/IEOT")
			}
			checkListing(t, r, name, ReadOptions{})
		})
	})
}

func TestReadSkipHash(t *testing.T) {
	r := populate(t, object.SHA1)
	name := r.path(".git/index")
	// git rewrites an index only when something changes, here the version.
	r.git("-c", "index.skipHash=true", "update-index", "--index-version", "4")
	if data := readFileBytes(t, name); !bytes.Equal(data[len(data)-20:], make([]byte, 20)) {
		t.Fatal("git computed a checksum")
	}
	idx := checkListing(t, r, name, ReadOptions{})
	if !idx.SkipHash {
		t.Fatal("SkipHash not recorded")
	}
	out := filepath.Join(t.TempDir(), "index")
	if err := WriteFile(out, idx, WriteOptions{Worktree: r.dir}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFileBytes(t, out), readFileBytes(t, name)) {
		t.Fatal("rewritten skip-hash index differs")
	}
}

func TestReadErrors(t *testing.T) {
	r := populate(t, object.SHA1)
	orig := readFileBytes(t, r.path(".git/index"))
	dir := t.TempDir()

	if _, err := Read(filepath.Join(dir, "missing"), object.SHA1, ReadOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}

	// withExtension appends an extension and an all-zero (index.skipHash)
	// checksum, which Read accepts without verification.
	withExtension := func(sig string, payload []byte) []byte {
		body := slices.Clone(orig[:len(orig)-20])
		body = append(body, sig...)
		body = binary.BigEndian.AppendUint32(body, uint32(len(payload)))
		body = append(body, payload...)
		return append(body, make([]byte, 20)...)
	}
	// hugeBitmap declares 2^32-1 bits in a 12-byte EWAH with no words.
	hugeBitmap := []byte{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0}
	fsmonitor := append([]byte{0, 0, 0, 2, 't', 0}, binary.BigEndian.AppendUint32(nil, uint32(len(hugeBitmap)))...)
	link := append(make([]byte, 20), append(slices.Clone(hugeBitmap), hugeBitmap...)...)
	corrupt := slices.Clone(orig)
	corrupt[len(corrupt)-1] ^= 1
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"optional extension", withExtension("ZZZZ", []byte("abc")), true},
		{"required extension", withExtension("zzzz", []byte("abc")), false},
		{"untracked ident unterminated", withExtension("UNTR", []byte{3, 'a', 'b', 'c'}), false},
		{"fsmonitor bitmap larger than index", withExtension("FSMN", append(fsmonitor, hugeBitmap...)), false},
		{"link bitmap larger than shared index", withExtension("link", link), false},
		{"checksum", corrupt, false},
		{"truncated", orig[:40], false},
		{"sha256 reader", orig, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := filepath.Join(dir, c.name)
			if err := os.WriteFile(name, c.data, 0o666); err != nil {
				t.Fatal(err)
			}
			f := object.SHA1
			if c.name == "sha256 reader" {
				f = object.SHA256
			}
			idx, err := Read(name, f, ReadOptions{})
			if c.ok != (err == nil) {
				t.Fatalf("err = %v", err)
			}
			if c.ok && len(idx.Entries) == 0 {
				t.Fatal("no entries")
			}
		})
	}
}
