package overlay

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type gatedBottom struct {
	dirBottom
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
	linkGate bool
}

func (b *gatedBottom) wait(p string) {
	if p == "slow" {
		b.once.Do(func() { close(b.started) })
		<-b.release
	}
}

func (b *gatedBottom) Open(p string) (Content, error) {
	b.wait(p)
	return b.dirBottom.Open(p)
}

func (b *gatedBottom) CopyTo(p, dst string) error {
	b.wait(p)
	return b.dirBottom.CopyTo(p, dst)
}

func (b *gatedBottom) Readlink(p string) (string, error) {
	if b.linkGate && p == "link" {
		b.wait("slow")
	}
	return b.dirBottom.Readlink(p)
}

// Copy-up must not publish stale bytes when another writer or a rebase wins
// while the bottom read is in flight.
func TestSlowCopyUpRechecksSource(t *testing.T) {
	for _, change := range []string{"write", "rebase"} {
		for _, memory := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/memory=%t", change, memory), func(t *testing.T) {
				lower := t.TempDir()
				must(t, os.WriteFile(filepath.Join(lower, "slow"), []byte("old"), 0o644))
				b := &gatedBottom{dirBottom: dirBottom{lower}, started: make(chan struct{}), release: make(chan struct{})}
				var release sync.Once
				unblock := func() { release.Do(func() { close(b.release) }) }
				t.Cleanup(unblock)
				var mem *Memory
				if memory {
					mem = NewMemory(1<<20, 1<<20, 0)
				}
				f, err := New(filepath.Join(t.TempDir(), "upper"), nil, b, mem)
				must(t, err)
				type result struct {
					data []byte
					err  error
				}
				done := make(chan result, 1)
				go func() {
					h, err := f.OpenFile("slow", os.O_RDWR, 0)
					if err != nil {
						done <- result{err: err}
						return
					}
					data, err := io.ReadAll(h)
					h.Close()
					done <- result{data: data, err: err}
				}()
				select {
				case <-b.started:
				case <-time.After(2 * time.Second):
					t.Fatal("copy-up did not start")
				}
				if change == "write" {
					write(t, f, "slow", "new", os.O_WRONLY|os.O_TRUNC)
				} else {
					next := t.TempDir()
					must(t, os.WriteFile(filepath.Join(next, "slow"), []byte("new"), 0o644))
					f.Rebase(dirBottom{next})
				}
				unblock()
				got := <-done
				if got.err != nil || string(got.data) != "new" || read(t, f, "slow") != "new" {
					t.Fatalf("open after %s: %q, %v", change, got.data, got.err)
				}
			})
		}
	}
}

func TestSlowBottomOpenDoesNotBlockOtherOperations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		flag     int
		memory   bool
		linkGate bool
	}{
		{"read", "slow", os.O_RDONLY, false, false},
		{"symlink", "link", os.O_RDONLY, false, false},
		{"symlink target", "link", os.O_RDONLY, false, true},
		{"write", "slow", os.O_RDWR, false, false},
		{"memory write", "slow", os.O_RDWR, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lower := t.TempDir()
			if err := os.WriteFile(filepath.Join(lower, "slow"), []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.path == "link" {
				if err := os.Symlink("slow", filepath.Join(lower, "link")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			b := &gatedBottom{dirBottom: dirBottom{lower}, started: make(chan struct{}), release: make(chan struct{}), linkGate: tc.linkGate}
			var release sync.Once
			unblock := func() { release.Do(func() { close(b.release) }) }
			t.Cleanup(unblock)
			var memory *Memory
			if tc.memory {
				memory = NewMemory(1<<20, 1<<20, 0)
			}
			f, err := New(filepath.Join(t.TempDir(), "upper"), nil, b, memory)
			if err != nil {
				t.Fatal(err)
			}
			opened := make(chan error, 1)
			go func() {
				h, err := f.OpenFile(tc.path, tc.flag, 0)
				if err == nil {
					_, err = io.ReadAll(h)
					h.Close()
				}
				opened <- err
			}()
			select {
			case <-b.started:
			case <-time.After(2 * time.Second):
				t.Fatal("bottom open did not start")
			}
			other := make(chan error, 1)
			go func() { other <- f.MkdirAll("independent", 0o755) }()
			select {
			case err := <-other:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Error("unrelated mkdir blocked on a bottom open")
			}
			unblock()
			if err := <-opened; err != nil {
				t.Error(err)
			}
		})
	}
}
