package store

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// prose returns n bytes of seeded pseudo-random words from a 1000-word
// vocabulary: compressible like source text, but not repetitive.
func prose(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	words := make([]string, 1000)
	for i := range words {
		w := make([]byte, 3+r.IntN(8))
		for j := range w {
			w[j] = byte('a' + r.IntN(26))
		}
		words[i] = string(w)
	}
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(words[r.IntN(len(words))])
		if r.IntN(12) == 0 {
			b.WriteByte('\n')
		} else {
			b.WriteByte(' ')
		}
	}
	return b.Bytes()[:n]
}

// edit returns a copy of data with a few bytes changed in the middle and a line appended.
func edit(data []byte, n int) []byte {
	out := bytes.Clone(data)
	copy(out[len(out)/2:], []byte("an edited line number "+string(rune('a'+n%26))))
	return append(out, []byte("appended line\n")...)
}

// putLikeFile writes data to a file and stores it with PutFileLike against like.
func putLikeFile(t *testing.T, s *Store, data []byte, like OID) OID {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	id, size, err := s.PutFileLike(p, like)
	if err != nil {
		t.Fatal(err)
	}
	if id != HashBytes("blob", data) || size != int64(len(data)) {
		t.Fatalf("PutFileLike = %s %d, want %s %d", id, size, HashBytes("blob", data), len(data))
	}
	return id
}

// TestPutFileLikeDeltaChain stores successive small edits of a large and of a
// small file as deltas costing bytes proportional to the edit, keeps chains
// within deltaDepth, and reads every version back, also after a reopen.
func TestPutFileLikeDeltaChain(t *testing.T) {
	for _, size := range []int{3 << 20, 200 << 10} {
		s := openStoreCache(t, 0)
		data := prose(uint64(size), size)
		like := putLikeFile(t, s, data, "")
		versions := map[OID][]byte{like: data}
		deltas := 0
		for i := range deltaDepth + 3 {
			data = edit(data, i)
			id := putLikeFile(t, s, data, like)
			versions[id] = data
			if depth := s.packs.depth(rawOf(id)); depth > deltaDepth {
				t.Fatalf("chain depth %d", depth)
			}
			if e, ok := s.packs.lookup(rawOf(id)); ok && e.flags&frameDelta != 0 {
				deltas++
				if e.stored > 4<<10 {
					t.Errorf("delta of a small edit to %d bytes stored in %d bytes", len(data), e.stored)
				}
			}
			like = id
		}
		if deltas < deltaDepth {
			t.Errorf("size %d: %d of %d versions stored as deltas", size, deltas, deltaDepth+3)
		}
		for _, st := range []*Store{s, reopen(t, s)} {
			for id, want := range versions {
				mustRead(t, st, id, want)
				dst := filepath.Join(t.TempDir(), "out")
				if err := st.CloneBlob(id, dst, 0o644); err != nil {
					t.Fatal(err)
				}
				if got, _ := os.ReadFile(dst); !bytes.Equal(got, want) {
					t.Fatal("clone of a delta differs")
				}
			}
		}
	}
}

// TestPutFileLikeFallsBack stores unrelated content, an unknown base and an
// unchanged file the way PutFile does.
func TestPutFileLikeFallsBack(t *testing.T) {
	s := openStore(t)
	base := putLikeFile(t, s, prose(1, 100<<10), "")
	other := prose(2, 100<<10)
	id := putLikeFile(t, s, other, base)
	if e, _ := s.packs.lookup(rawOf(id)); e.flags&frameDelta != 0 {
		t.Error("unrelated content stored as a delta")
	}
	missing := putLikeFile(t, s, prose(3, 1000), HashBytes("blob", []byte("never stored")))
	if e, _ := s.packs.lookup(rawOf(missing)); e.flags&frameDelta != 0 {
		t.Error("delta against a missing base")
	}
	if again := putLikeFile(t, s, other, base); again != id {
		t.Fatal("re-storing changed the id")
	}
	if got, ok := s.Cached(id); !ok || !bytes.Equal(got, other) {
		t.Fatal("Cached of a plain object")
	}
}

// TestSweepKeepsDeltaBases keeps the (old, unreachable) bases of a kept delta
// chain, the first packed or loose, and removes them all once the delta goes.
func TestSweepKeepsDeltaBases(t *testing.T) {
	for _, size := range []int{200 << 10, 2 << 20} {
		s := openStore(t)
		baseData := prose(9, size)
		base := putLikeFile(t, s, baseData, "")
		middle := putLikeFile(t, s, edit(baseData, 1), base)
		next := edit(edit(baseData, 1), 2)
		delta := putLikeFile(t, s, next, middle)
		if s.packs.depth(rawOf(delta)) != 2 {
			t.Fatal("edits not stored as a delta chain")
		}
		root, err := s.PutTree([]Entry{{Name: "f", Mode: ModeFile, ID: delta}})
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		for _, id := range []OID{base, middle, delta, root} {
			age(t, s, id, old)
		}
		if n, err := s.Sweep([]OID{root}, time.Now().Add(-time.Minute)); err != nil || n != 0 {
			t.Fatalf("Sweep removed %d objects (%v) while the delta is live", n, err)
		}
		mustRead(t, s, delta, next)
		if n, err := s.Sweep(nil, time.Now().Add(-time.Minute)); err != nil || n != 4 {
			t.Fatalf("Sweep of an unreachable delta removed %d objects, %v; want 4", n, err)
		}
		if s.Has(base) || s.Has(middle) || s.Has(delta) {
			t.Fatal("unreachable delta or its base kept")
		}
	}
}
