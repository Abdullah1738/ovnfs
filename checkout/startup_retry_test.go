package checkout

import (
	"context"
	"errors"
	"fmt"
	"github.com/stencil-hq/ovnfs/api"
	"os"
	"path/filepath"
	"testing"
)

func TestUncommittedNameReportsBusy(t *testing.T) {
	f := &fixture{reg: &Registry{reserved: map[string]bool{}, checkouts: map[string]*checkoutRecord{}}}
	for _, row := range []struct {
		name                string
		reserved, committed bool
		code                string
	}{
		{"free", false, false, ""}, {"reserved", true, false, "busy"}, {"committed", false, true, "exists"}, {"pending-publication", true, true, "busy"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.reserved {
				f.reg.reserved[row.name] = true
			}
			if row.committed {
				f.reg.checkouts[row.name] = &checkoutRecord{}
			}
			_, err := f.reg.pickNameLocked(row.name, "")
			if row.code == "" && err != nil {
				t.Fatal(err)
			}
			var codedError *Error
			if row.code != "" && (!errors.As(err, &codedError) || codedError.Code != row.code) {
				t.Fatalf("got %v, want %s", err, row.code)
			}
			delete(f.reg.reserved, row.name)
			delete(f.reg.checkouts, row.name)
		})
	}
}

func TestInterruptedCreateHandoffAllowsSameNameRetry(t *testing.T) {
	f := newFixture(t)
	name := "interrupted"
	dir := folder(t, map[string]string{"file": "source"})
	// Preserve precisely the durable intent a killed create leaves before its
	// final state commit, including partial upper and git directories.
	f.reg.mu.Lock()
	f.reg.reserved[name] = true
	f.reg.intents[name] = diskIntent{Op: "creating", Checkout: name}
	if err := f.reg.persistIntentLocked(); err != nil {
		t.Fatal(err)
	}
	f.reg.mu.Unlock()
	for _, sub := range []string{"uppers", "git"} {
		p := filepath.Join(f.state, sub, name)
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "partial"), []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if len(f.reg.checkouts) != 0 || len(f.reg.reserved) != 0 {
		t.Fatal("uncommitted name survived handoff")
	}
	reply, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Checkout.Name != name {
		t.Fatalf("retry name=%s", reply.Checkout.Name)
	}
	if got := f.read(name, "file"); got != "source" {
		t.Fatalf("retry content=%q", got)
	}
}

func TestLoadingProgressCompletes(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"file": "source"}), "one")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	n, total := -1, -1
	reg, err := Open(Config{State: f.state, Mount: f.mount, Loading: func(done, count int) {
		if done < n || done > count {
			t.Fatalf("invalid progress %d/%d after %d", done, count, n)
		}
		n, total = done, count
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if total < 2 || n != total {
		t.Fatalf("final loading=%d/%d", n, total)
	}
}

func TestPendingPublicationIsBusyUntilCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			f := newFixture(t)
			dir := folder(t, map[string]string{"file": "source"})
			first := f.newCheckout(dir, "base").Checkout
			f.reg.mu.Lock()
			b := f.reg.bases[first.Base]
			f.reg.mu.Unlock()
			f.reg.commits.mu.Lock()
			f.reg.commits.running = true
			f.reg.commits.mu.Unlock()
			result := make(chan error, 1)
			go func() {
				f.reg.mu.Lock()
				_, err := f.reg.openOnLocked(b, gitTemplate{}, "pending", "", nil)
				result <- err
			}()
			eventually(t, "publication did not reach commit", func() bool { f.reg.mu.RLock(); defer f.reg.mu.RUnlock(); return f.reg.checkouts["pending"] != nil })
			f.reg.mu.Lock()
			_, err := f.reg.pickNameLocked("pending", "")
			f.reg.mu.Unlock()
			var e *Error
			if _, newErr := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: "pending"}); !errors.As(newErr, &e) || e.Code != "busy" {
				t.Fatalf("concurrent same-name create: %v", newErr)
			}
			if removeErr := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "pending"}); !errors.As(removeErr, &e) || e.Code != "busy" {
				t.Fatalf("removed pending publication: %v", removeErr)
			}
			if !errors.As(err, &e) || e.Code != "busy" {
				t.Fatalf("pending commit returned %v", err)
			}
			f.reg.commits.mu.Lock()
			f.reg.commits.running = false
			if fail {
				f.reg.commits.failed = f.reg.commits.want
				f.reg.commits.err = errors.New("injected commit failure")
			}
			f.reg.commits.cond.Broadcast()
			f.reg.commits.mu.Unlock()
			err = <-result
			if fail && err == nil {
				t.Fatal("commit failure ignored")
			}
			if !fail && err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: "pending"}); err != nil {
					t.Fatalf("same-name retry: %v", err)
				}
			} else {
				f.reg.mu.Lock()
				_, err = f.reg.pickNameLocked("pending", "")
				f.reg.mu.Unlock()
				if !errors.As(err, &e) || e.Code != "exists" {
					t.Fatalf("durable returned %v", err)
				}
			}
		})
	}
}
