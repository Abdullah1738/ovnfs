package object

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteLooseMatchesHashObject(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			r := newRepo(t, "--object-format="+format)
			db := r.open()
			blob := []byte("hello\nworld\n")
			tree := EncodeTree([]TreeEntry{{Name: "hello", Mode: ModeFile, ID: Hash(r.format, Blob, blob)}})
			commit := []byte("tree " + Hash(r.format, Tree, tree).String() +
				"\nauthor A <a@example.com> 1700000000 +0000\ncommitter A <a@example.com> 1700000000 +0000\n\nmsg\n")
			for _, obj := range []struct {
				kind Kind
				data []byte
			}{{Blob, nil}, {Blob, blob}, {Blob, bytes.Repeat([]byte{0, 1, 2, 255}, 100000)}, {Tree, tree}, {Commit, commit}} {
				want := r.id(string(r.git(obj.data, "hash-object", "-t", obj.kind.String(), "--stdin")))
				if got := Hash(r.format, obj.kind, obj.data); got != want {
					t.Errorf("Hash = %s, want %s", got, want)
				}
				id, err := db.WriteLoose(obj.kind, obj.data)
				if err != nil || id != want {
					t.Fatalf("WriteLoose = %s %v, want %s", id, err, want)
				}
				path := loosePath(r.objects(), id)
				fi, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && fi.Mode().Perm()&0o222 != 0 {
					t.Errorf("loose object mode %v is writable", fi.Mode())
				}
				if got := r.git(nil, "cat-file", obj.kind.String(), id.String()); !bytes.Equal(got, obj.data) {
					t.Errorf("git cat-file of written object differs")
				}
			}
			r.git(nil, "fsck", "--full", "--no-dangling")
			entries, _ := os.ReadDir(filepath.Dir(loosePath(r.objects(), Hash(r.format, Blob, blob))))
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "tmp_") {
					t.Errorf("temporary file %s left behind", e.Name())
				}
			}
		})
	}
}

func TestWriteLooseKeepsExistingObject(t *testing.T) {
	r := newRepo(t)
	data := []byte("existing\n")
	id := r.id(string(r.git(data, "hash-object", "-w", "--stdin")))
	path := loosePath(r.objects(), id)
	before, _ := os.ReadFile(path)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(path, old, old)

	db := r.open()
	if got, err := db.WriteLoose(Blob, data); err != nil || got != id {
		t.Fatalf("WriteLoose = %s %v", got, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("existing loose object was rewritten")
	}
	if fi, _ := os.Stat(path); fi.ModTime().Before(time.Now().Add(-time.Hour)) {
		t.Error("existing loose object was not freshened")
	}

	// An object only in a pack is not written loose again.
	packed := []byte("packed\n")
	pid := r.id(string(r.git(packed, "hash-object", "-w", "--stdin")))
	r.git(nil, "repack", "-a", "-d", "-q", "-k")
	r.git(nil, "prune-packed")
	db2 := r.open()
	if got, err := db2.WriteLoose(Blob, packed); err != nil || got != pid {
		t.Fatalf("WriteLoose = %s %v", got, err)
	}
	if _, err := os.Stat(loosePath(r.objects(), pid)); err == nil {
		t.Error("packed object was written loose")
	}
}

func TestPackWriter(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			src := newRepo(t, "--object-format="+format)
			src.fastImport(history(src.format, 20, "pw"))
			objs := src.allObjects()

			dst := newRepo(t, "--bare", "--object-format="+format)
			packDir := filepath.Join(dst.objects(), "pack")
			w, err := NewPackWriter(packDir, dst.format)
			if err != nil {
				t.Fatal(err)
			}
			for id, o := range objs {
				for range 2 { // the second Add is deduplicated
					got, err := w.Add(o.kind, o.data)
					if err != nil || got != id {
						t.Fatalf("Add = %s %v, want %s", got, err, id)
					}
				}
			}
			name, err := w.Commit()
			if err != nil {
				t.Fatal(err)
			}
			idx := filepath.Join(packDir, "pack-"+name+".idx")
			out := dst.gitString("verify-pack", "-v", idx)
			if n := strings.Count(out, "\n") - 2; n < len(objs) { // object lines plus summary lines
				t.Errorf("verify-pack lists %d lines for %d objects", n, len(objs))
			}
			dst.git(nil, "update-ref", "refs/heads/main", src.gitString("rev-parse", "main")[:dst.format.HexSize()])
			dst.git(nil, "fsck", "--full", "--strict")
			checkAllObjects(t, dst.open(), objs)

			// git's index of our pack is byte-identical to ours.
			ours, _ := os.ReadFile(idx)
			theirs := filepath.Join(t.TempDir(), "git.idx")
			dst.git(nil, "index-pack", "-o", theirs, filepath.Join(packDir, "pack-"+name+".pack"))
			if b, _ := os.ReadFile(theirs); !bytes.Equal(ours, b) {
				t.Error("index differs from git index-pack's")
			}
			assertNoTemp(t, packDir)
		})
	}
}

func TestPackWriterEmptyAndAbort(t *testing.T) {
	dir := t.TempDir()
	w, err := NewPackWriter(dir, SHA1)
	if err != nil {
		t.Fatal(err)
	}
	if name, err := w.Commit(); err != nil || name != "" {
		t.Errorf("empty Commit = %q %v", name, err)
	}
	w, _ = NewPackWriter(dir, SHA1)
	w.Add(Blob, []byte("x"))
	w.Abort()
	if _, err := w.Add(Blob, []byte("y")); err == nil {
		t.Error("Add after Abort succeeded")
	}
	assertNoTemp(t, dir)
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left: %v", ents)
	}
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "tmp_") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}
