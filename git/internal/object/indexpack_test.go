package object

import (
	"bytes"
	"compress/zlib"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexPackFullPacks(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		for _, flags := range [][]string{{"--delta-base-offset"}, nil} {
			name := format + " ofs-delta"
			if flags == nil {
				name = format + " ref-delta"
			}
			t.Run(name, func(t *testing.T) {
				src := newRepo(t, "--object-format="+format)
				src.fastImport(history(src.format, 40, "ip"))
				src.git(nil, "tag", "-a", "-m", "nested", "nested", "v10")
				pack := src.git([]byte{}, append([]string{"pack-objects", "--all", "--stdout", "-q"}, flags...)...)
				objs := src.allObjects()

				dst := newRepo(t, "--bare", "--object-format="+format)
				packDir := filepath.Join(dst.objects(), "pack")
				name, n, err := IndexPack(bytes.NewReader(pack), packDir, dst.format, IndexPackOptions{DB: dst.open()})
				if err != nil {
					t.Fatal(err)
				}
				if n != len(objs) {
					t.Errorf("IndexPack counted %d objects, want %d", n, len(objs))
				}
				base := filepath.Join(packDir, "pack-"+name)
				if got, _ := os.ReadFile(base + ".pack"); !bytes.Equal(got, pack) {
					t.Error("stored pack differs from the received stream")
				}
				dst.git(nil, "verify-pack", base+".idx")
				ours, _ := os.ReadFile(base + ".idx")
				theirs := filepath.Join(t.TempDir(), "git.idx")
				dst.git(nil, "index-pack", "-o", theirs, base+".pack")
				if b, _ := os.ReadFile(theirs); !bytes.Equal(ours, b) {
					t.Error("index differs from git index-pack's")
				}
				dst.git(nil, "update-ref", "refs/heads/main", strings.TrimSpace(src.gitString("rev-parse", "main")))
				dst.git(nil, "fsck", "--full", "--strict")
				checkAllObjects(t, dst.open(), objs)
				assertNoTemp(t, packDir)

				// Indexing the same pack again keeps the installed files.
				if again, _, err := IndexPack(bytes.NewReader(pack), packDir, dst.format, IndexPackOptions{}); err != nil || again != name {
					t.Errorf("second IndexPack = %s %v", again, err)
				}
			})
		}
	}
}

func TestIndexPackThin(t *testing.T) {
	src := newRepo(t)
	src.fastImport(history(src.format, 30, "thin"))
	old := strings.TrimSpace(src.gitString("rev-parse", "main~10"))
	head := strings.TrimSpace(src.gitString("rev-parse", "main"))

	// dst has everything up to main~10.
	dst := newRepo(t, "--bare")
	dst.git(src.git([]byte(old+"\n"), "pack-objects", "--revs", "--stdout", "-q"), "index-pack", "--stdin")
	dst.git(nil, "update-ref", "refs/heads/main", old)

	thin := src.git([]byte(head+"\n^"+old+"\n"), "pack-objects", "--revs", "--thin", "--stdout", "-q", "--delta-base-offset")
	packDir := filepath.Join(dst.objects(), "pack")
	db := dst.open()
	if _, _, err := IndexPack(bytes.NewReader(thin), packDir, dst.format, IndexPackOptions{DB: db}); err == nil {
		t.Fatal("thin pack indexed without FixThin")
	}
	assertNoTemp(t, packDir)
	name, n, err := IndexPack(bytes.NewReader(thin), packDir, dst.format, IndexPackOptions{DB: db, FixThin: true})
	if err != nil {
		t.Fatal(err)
	}
	inPack := int(uint32(thin[8])<<24 | uint32(thin[9])<<16 | uint32(thin[10])<<8 | uint32(thin[11]))
	if n <= inPack {
		t.Fatalf("fixed pack has %d objects, thin pack %d: no bases appended", n, inPack)
	}
	idx := filepath.Join(packDir, "pack-"+name+".idx")
	if out := dst.gitString("verify-pack", "-v", idx); !strings.Contains(out, ".pack: ok") {
		t.Errorf("verify-pack:\n%s", out)
	}
	dst.git(nil, "update-ref", "refs/heads/main", head)
	dst.git(nil, "fsck", "--full", "--strict")
	checkAllObjects(t, dst.open(), dst.allObjects())
}

func TestIndexPackRejects(t *testing.T) {
	src := newRepo(t)
	src.fastImport(history(src.format, 5, "bad"))
	pack := src.git([]byte{}, "pack-objects", "--all", "--stdout", "-q")

	t.Run("checksum", func(t *testing.T) {
		bad := bytes.Clone(pack)
		bad[len(bad)-1] ^= 1
		if _, _, err := IndexPack(bytes.NewReader(bad), t.TempDir(), SHA1, IndexPackOptions{}); err == nil {
			t.Error("corrupt checksum accepted")
		}
	})
	t.Run("truncated", func(t *testing.T) {
		dir := t.TempDir()
		if _, _, err := IndexPack(bytes.NewReader(pack[:len(pack)/2]), dir, SHA1, IndexPackOptions{}); err == nil {
			t.Error("truncated pack accepted")
		}
		assertNoTemp(t, dir)
	})
	t.Run("collision", func(t *testing.T) {
		dst := newRepo(t, "--bare")
		// Plant a loose object under the name of a blob in the pack but
		// with different content, as a hash collision would.
		blob := src.id(src.gitString("rev-parse", "main:bin/tool"))
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		zw.Write([]byte("blob 5\x00other"))
		zw.Close()
		path := loosePath(dst.objects(), blob)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, buf.Bytes(), 0o444)
		_, _, err := IndexPack(bytes.NewReader(pack), filepath.Join(dst.objects(), "pack"), SHA1, IndexPackOptions{DB: dst.open()})
		if err == nil || !strings.Contains(err.Error(), "collision") {
			t.Errorf("IndexPack error %v, want a collision", err)
		}
	})
}
