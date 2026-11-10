package export

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A client may retain just one handle in each of several wide directories.
// Recovering every owner on each alternating GETATTR makes work proportional
// to directory width even though only four names are requested.
func TestSparseColdHandles(t *testing.T) {
	for _, budget := range []int64{0, 4096, 1 << 20, 16 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ns := newNamespace("pressure/entry")
			nt := newNodeTable(ns.list)
			defer nt.close()
			var ids []uint64
			var paths []string
			for d := range 4 {
				for f := range 4096 {
					ns.add(fmt.Sprintf("d%d/f%04d", d, f))
				}
			}
			for d := range 4 {
				for f := range 4096 {
					p := fmt.Sprintf("d%d/f%04d", d, f)
					id := nt.id(p)
					if f == 0 {
						ids = append(ids, id)
						paths = append(paths, p)
					}
				}
			}
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			// Keep a real listing pinned just as mutation preparation does. This
			// consumes the larger allowance without allowing trim to free it and
			// accidentally turning this into an under-budget batch test.
			if budget > 4096 {
				nt.mu.Lock()
				d := nt.find("pressure")
				for i := 0; nt.cacheUsed < budget-4096; i++ {
					nt.addLeaf(d, fmt.Sprintf("p%06d-%s", i, strings.Repeat("p", 232)))
				}
				d.dir.reading++
				nt.mu.Unlock()
				defer func() { d.dir.reading-- }()
			}
			nt.cacheLimit = budget
			var reads atomic.Int32
			nt.indexIO = func() { reads.Add(1) }
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			for i := range 512 {
				j := i % len(ids)
				if got := pathOf(t, nt, ids[j]); got != paths[j] {
					t.Fatalf("%q != %q", got, paths[j])
				}
			}
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("512 alternating handles: %s allocated=%d index_operations=%d", time.Since(start), allocated, reads.Load())
			if allocated > 4<<20 {
				t.Fatalf("sparse recovery allocated %d bytes", allocated)
			}
			maxReads := int32(4)
			if budget == 0 {
				maxReads = 514
			} // one owner search per request, plus spill publication
			if reads.Load() > maxReads {
				t.Fatalf("%d index operations, want <= %d", reads.Load(), maxReads)
			}
			for d := range 4 {
				s := nt.find(fmt.Sprintf("d%d", d)).dir
				if s.coldLoaded || len(s.leaves) > 1 {
					t.Fatal("sparse recovery admitted unrelated owners")
				}
			}
		})
	}
}

// Exercise the partial-read validation boundary, including ownership changes
// that do not change the namespace generation.
func TestSparseColdReadRevalidated(t *testing.T) {
	for _, change := range []string{"directory", "retired", "owner", "move"} {
		t.Run(change, func(t *testing.T) {
			ns := newNamespace("co/file", "pressure/entry")
			nt := newNodeTable(ns.list)
			old := nt.id("co/file")
			forceCold(t, nt, ns, "co")
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
			done := make(chan string, 1)
			go func() {
				parts, ok := nt.path(old)
				if !ok {
					done <- ""
				} else {
					done <- strings.Join(parts, "/")
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("read did not start")
			}
			nt.mu.Lock()
			d := nt.find("co")
			switch change {
			case "directory":
				nt.drop(d)
			case "retired":
				d.dir.retired = map[uint32]struct{}{nameHash("file"): {}}
			case "owner":
				nt.addLeaf(d, "file")
				nt.child(d, "file")
			case "move":
				d.moves++
				delete(d.parent.kids, d.name)
				d.name = "moved"
				d.parent.kids[d.name] = d
			}
			nt.mu.Unlock()
			close(resume)
			want := ""
			if change == "owner" {
				want = "co/file"
			}
			if change == "move" {
				want = "moved/file"
			}
			select {
			case got := <-done:
				if got != want {
					t.Fatalf("got %q want %q", got, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("read stuck")
			}
			if nt.cacheErr != nil {
				t.Fatal(nt.cacheErr)
			}
		})
	}
}

func TestSparseColdPreRestartHandle(t *testing.T) {
	for _, budget := range []int64{0, 1 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ns := newNamespace("co/first", "co/second", "pressure/entry")
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, log, ns)
			first, second := nt.id("co/first"), nt.id("co/second")
			closeLogged(t, nt)
			nt = openLogged(t, log, ns)
			if got := pathOf(t, nt, first); got != "co/first" {
				t.Fatal(got)
			}
			forceCold(t, nt, ns, "co")
			if budget != 0 {
				d := nt.find("pressure")
				for i := 0; nt.cacheUsed < budget-4096; i++ {
					nt.addLeaf(d, fmt.Sprintf("p%06d-%s", i, strings.Repeat("p", 232)))
				}
				d.dir.reading++
				defer func() { d.dir.reading-- }()
			}
			nt.cacheLimit = budget
			if got := pathOf(t, nt, second); got != "co/second" {
				t.Fatalf("pre-restart handle: %q", got)
			}
			if got := pathOf(t, nt, first); got != "co/first" {
				t.Fatal(got)
			}
			if nt.cacheErr != nil {
				t.Fatal(nt.cacheErr)
			}
		})
	}
}
