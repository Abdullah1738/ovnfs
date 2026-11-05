package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/internal/search/grep"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/peer"
	"github.com/stencil-hq/ovnfs/store"
)

// offerFixture attaches source metadata and seeds blobs without requiring a mounted export.
func offerFixture(t *testing.T, src, dst *fixture, name string) (peer.Offer, api.Checkout) {
	t.Helper()
	o, blobs, _, err := src.reg.Offer(context.Background(), name, name)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range o.Files {
		if e.Mode == store.ModeDir {
			continue
		}
		f, _, err := blobs.Open(e.ID, e.Path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dst.reg.store.PutBytes(data); err != nil {
			t.Fatal(err)
		}
	}
	h, err := dst.reg.AttachRemote(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return o, h
}

// TestReattachKeepsCacheAndHandles exercises a warm lane both before and after a snapshot.
func TestReattachKeepsCacheAndHandles(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(map[bool]string{false: "upper", true: "snapshot"}[frozen], func(t *testing.T) {
			src, dst := newFixture(t), newFixture(t)
			dir := folder(t, map[string]string{"src/main": "old", "target/seed": "seed"})
			src.newCheckout(dir, "app", "cache dir & target")
			_, first := offerFixture(t, src, dst, "app")
			dst.put(first.Name, "target/cache", "warm")
			dst.put(first.Name, ".env", "machine-local")
			if frozen {
				if _, err := dst.reg.Snap(context.Background(), api.SnapArgs{Checkout: first.Name, Wait: true}); err != nil {
					t.Fatal(err)
				}
			}
			s, err := dst.reg.Enter(first.Name)
			if err != nil {
				t.Fatal(err)
			}
			fs, gen := s.FS, s.Gen
			handle, err := fs.Open("target/cache")
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			cwd, err := fs.Chroot("target")
			s.Done()
			if err != nil {
				t.Fatal(err)
			}
			src.put("app", "src/main", "new")
			_, second := offerFixture(t, src, dst, "app")
			if second.Name != first.Name || second.Path != first.Path {
				t.Fatalf("lane moved: %+v -> %+v", first, second)
			}
			if !slices.ContainsFunc(dst.invalidations(), func(i invalidation) bool { return i.checkout == second.Name && i.paths == nil }) {
				t.Fatal("reattach swapped the base without invalidating the checkout")
			}
			s, err = dst.reg.Enter(second.Name)
			if err != nil {
				t.Fatal(err)
			}
			if s.FS != fs || s.Gen != gen {
				t.Fatal("reattach invalidated the lane's filesystem or node generation")
			}
			s.Done()
			if got := dst.read(second.Name, "src/main"); got != "new" {
				t.Fatalf("source = %q", got)
			}
			if got := dst.read(second.Name, "target/cache"); got != "warm" {
				t.Fatalf("cache = %q", got)
			}
			if got := dst.read(second.Name, ".env"); got != "machine-local" {
				t.Fatalf("local = %q", got)
			}
			data, err := io.ReadAll(handle)
			if err != nil || string(data) != "warm" {
				t.Fatalf("open handle = %q, %v", data, err)
			}
			rel, err := cwd.Open("cache")
			if err != nil {
				t.Fatal(err)
			}
			rel.Close()
			if err := dst.reg.Close(); err != nil {
				t.Fatal(err)
			}
			dst.open()
			if got := dst.read(second.Name, "target/cache"); got != "warm" {
				t.Fatalf("cache after reopen = %q", got)
			}
			if got := dst.read(second.Name, "src/main"); got != "new" {
				t.Fatalf("source after reopen = %q", got)
			}
		})
	}
}

// TestReattachPreservesSourceEdits keeps a modified remote lane beside a new offer.
func TestReattachPreservesSourceEdits(t *testing.T) {
	src, dst := newFixture(t), newFixture(t)
	src.newCheckout(folder(t, map[string]string{"main": "old"}), "app")
	_, first := offerFixture(t, src, dst, "app")
	dst.put(first.Name, "main", "remote edit")
	src.put("app", "main", "new source")
	_, second := offerFixture(t, src, dst, "app")
	if second.Name == first.Name || second.Path == first.Path {
		t.Fatal("source-modified lane was replaced")
	}
	if got := dst.read(first.Name, "main"); got != "remote edit" {
		t.Fatalf("remote edit = %q", got)
	}
}

// TestLegacyStacksReopen retains compatibility with version-four manifests without explicit stacks.
func TestLegacyStacksReopen(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"file": "base"}), "app")
	f.put("app", "file", "changed")
	if _, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "app", Wait: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.state, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d diskState
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	for i := range d.Checkouts {
		d.Checkouts[i].Stack = nil
	}
	for i := range d.Snapshots {
		d.Snapshots[i].Stack = nil
	}
	if err := writeState(f.state, d); err != nil {
		t.Fatal(err)
	}
	f.open()
	if got := f.read("app", "file"); got != "changed" {
		t.Fatalf("legacy state = %q", got)
	}
}

// TestPullMergesIntoLocalCheckout covers direct changes, clean merges, conflict markers,
// untouched conflicts, idempotence and a resolution that later pulls keep.
func TestPullMergesIntoLocalCheckout(t *testing.T) {
	isolateGit(t)
	src, dst := newFixture(t), newFixture(t)
	base := "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"
	dir := folder(t, map[string]string{"direct": "before", "merge": base, "conflict": "before", "delete": "remove", "binary": "base\x00"})
	src.newCheckout(dir, "app")
	offer, remote := offerFixture(t, src, dst, "app")
	dst.put(remote.Name, "direct", "remote")
	dst.put(remote.Name, "merge", "remote\n"+"two\nthree\nfour\nfive\nsix\nseven\n")
	dst.put(remote.Name, "conflict", "remote")
	dst.put(remote.Name, "new", "added")
	dst.put(remote.Name, "binary", "remote\x00")
	dst.remove(remote.Name, "delete")
	src.put("app", "merge", "one\ntwo\nthree\nfour\nfive\nsix\nlocal\n")
	src.put("app", "conflict", "local")
	src.put("app", "binary", "local\x00")
	pulled, err := dst.reg.RemoteChanges(context.Background(), remote.Name, offer.Tree)
	if err != nil {
		t.Fatal(err)
	}
	dry, err := src.reg.MergePull(context.Background(), api.PullArgs{Checkout: "app", DryRun: true}, offer.Tree, pulled)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Applied) != 3 || len(dry.Merged) != 1 || len(dry.Conflicts) != 2 {
		t.Fatalf("dry run = %+v", dry)
	}
	if got := src.read("app", "direct"); got != "before" {
		t.Fatalf("dry run wrote direct = %q", got)
	}
	if got := src.read("app", "new"); got != "<absent>" {
		t.Fatalf("dry run added new = %q", got)
	}
	if got := src.read("app", "conflict"); got != "local" {
		t.Fatalf("dry run marked conflict = %q", got)
	}
	if got := src.invalidations(); len(got) != 0 {
		t.Fatalf("dry run invalidated %+v", got)
	}
	result, err := src.reg.MergePull(context.Background(), api.PullArgs{Checkout: "app"}, offer.Tree, pulled)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 3 || len(result.Merged) != 1 || len(result.Conflicts) != 2 || !result.Conflicts[1].Markers || result.Conflicts[0].Markers {
		t.Fatalf("pull = %+v", result)
	}
	got := src.invalidations()
	if len(got) != 1 || got[0].checkout != "app" {
		t.Fatalf("pull invalidations = %+v", got)
	}
	if paths := slices.Sorted(slices.Values(got[0].paths)); !slices.Equal(paths, []string{"conflict", "delete", "direct", "merge", "new"}) {
		t.Fatalf("pull invalidated %v, want exactly the written paths", paths)
	}
	for p, want := range map[string]string{
		"direct": "remote", "new": "added", "delete": "<absent>",
		"merge": "remote\ntwo\nthree\nfour\nfive\nsix\nlocal\n", "binary": "local\x00",
		"conflict": "<<<<<<< local\nlocal\n=======\nremote\n>>>>>>> remote " + remote.Name + "\n",
	} {
		if got := src.read("app", p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "direct"))
	if err != nil || string(data) != "before" {
		t.Fatalf("pull touched source folder: %q, %v", data, err)
	}
	again, err := src.reg.MergePull(context.Background(), api.PullArgs{Checkout: "app"}, offer.Tree, pulled)
	if err != nil || len(again.Applied) != 0 || len(again.Merged) != 0 || len(again.Conflicts) != 2 || !again.Conflicts[1].Markers {
		t.Fatalf("second pull = %+v, %v", again, err)
	}
	if n := len(src.invalidations()); n != 1 {
		t.Fatalf("a pull that wrote nothing invalidated (%d calls)", n)
	}
	src.put("app", "conflict", "resolved")
	resolved, err := src.reg.MergePull(context.Background(), api.PullArgs{Checkout: "app"}, offer.Tree, pulled)
	if err != nil || len(resolved.Applied) != 0 || len(resolved.Merged) != 0 || len(resolved.Conflicts) != 1 || resolved.Conflicts[0].Path != "binary" {
		t.Fatalf("pull after resolving = %+v, %v", resolved, err)
	}
	if got := src.read("app", "conflict"); got != "resolved" {
		t.Fatalf("pull merged the resolved conflict again: %q", got)
	}
}

// TestPullRejectsCorruptAndRedirectedPaths prevents remote bytes escaping the lane.
func TestPullRejectsCorruptAndRedirectedPaths(t *testing.T) {
	src, dst := newFixture(t), newFixture(t)
	src.newCheckout(folder(t, map[string]string{"file": "base"}), "app")
	offer, remote := offerFixture(t, src, dst, "app")
	dst.put(remote.Name, "file", "remote")
	pulled, err := dst.reg.RemoteChanges(context.Background(), remote.Name, offer.Tree)
	if err != nil {
		t.Fatal(err)
	}
	pulled.Data[pulled.Files["file"].ID] = []byte("forged")
	if _, err := src.reg.MergePull(context.Background(), api.PullArgs{Checkout: "app"}, offer.Tree, pulled); err == nil {
		t.Fatal("accepted corrupt content")
	}
	if got := src.read("app", "file"); got != "base" {
		t.Fatalf("corrupt pull wrote %q", got)
	}
	id := store.HashBytes("blob", []byte("remote"))
	pulled = peer.Pulled{Base: map[string]store.File{}, Files: map[string]store.File{"../escape": {Mode: store.ModeFile, ID: id}}, Data: map[store.OID][]byte{id: []byte("remote")}}
	if _, err := src.reg.MergePull(context.Background(), api.PullArgs{Checkout: "app"}, offer.Tree, pulled); err == nil {
		t.Fatal("accepted a traversal path")
	}
	s, err := src.reg.Enter("app")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	err = s.FS.Symlink(outside, "redirect")
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	pulled.Files = map[string]store.File{"redirect/file": {Mode: store.ModeFile, ID: id}}
	result, err := src.reg.MergePull(context.Background(), api.PullArgs{Checkout: "app"}, offer.Tree, pulled)
	if err != nil || len(result.Conflicts) != 1 {
		t.Fatalf("redirect pull = %+v, %v", result, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote path escaped the checkout: %v", err)
	}
}

// countingBlobs records which blobs a source served.
type countingBlobs struct {
	peer.Blobs
	mu     sync.Mutex
	served map[store.OID]int
}

// Open counts and forwards.
func (c *countingBlobs) Open(id store.OID, p string) (io.ReadCloser, int64, error) {
	c.mu.Lock()
	c.served[id]++
	c.mu.Unlock()
	return c.Blobs.Open(id, p)
}

// remoteLink is a registry holding a checkout attached from another registry
// over an in-memory link that serves its fetches and searches.
type remoteLink struct {
	reg    *Registry
	name   string     // the attached checkout
	offer  peer.Offer // as the remote accepted it
	served *countingBlobs
	stop   func() // ends the link; reads then see only cached content
}

// attachOverPipe attaches src's checkout to a new registry over net.Pipe.
func attachOverPipe(t *testing.T, src *fixture, checkout string) *remoteLink {
	t.Helper()
	offer, blobs, _, err := src.reg.Offer(context.Background(), checkout, "")
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingBlobs{Blobs: blobs, served: map[store.OID]int{}}
	var sessions sync.Map
	session := func(key string, wants []base.Want) (*peer.Session, []peer.Want, error) {
		s, ok := sessions.Load(key)
		if !ok {
			return nil, nil, errors.New("not connected")
		}
		pw := make([]peer.Want, len(wants))
		for i, w := range wants {
			pw[i] = peer.Want{ID: w.ID, Path: w.Path, Size: w.Size}
		}
		return s.(*peer.Session), pw, nil
	}
	reg, err := Open(Config{State: filepath.Join(t.TempDir(), "state"), Mount: filepath.Join(t.TempDir(), "mount"), Memory: overlay.NewMemory(64<<20, 1<<20, 0), BlobCache: 64 << 20,
		Fetch: func(ctx context.Context, key string, wants []base.Want) error {
			s, pw, err := session(key, wants)
			if err != nil {
				return err
			}
			return s.Fetch(ctx, pw)
		},
		Search: func(ctx context.Context, key string, q grep.Query, wants []base.Want) ([]grep.Outcome, error) {
			s, pw, err := session(key, wants)
			if err != nil {
				return nil, err
			}
			return s.Search(ctx, q, pw)
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })

	left, right := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- peer.Serve(ctx, left, offer, counter, nil) }()
	s, got, err := peer.Accept(right, reg.Store())
	if err != nil {
		t.Fatal(err)
	}
	h, err := reg.AttachRemote(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	key := LinkKey(got.Node, got.Source)
	sessions.Store(key, s)
	if err := s.Reply(peer.Accepted{Checkout: h.Name, Path: h.Path}); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() {
		cancel()
		<-served
		sessions.Delete(key)
	})
	t.Cleanup(stop)
	return &remoteLink{reg: reg, name: h.Name, offer: got, served: counter, stop: stop}
}

// servedCount is how many distinct blobs the source has served so far.
func (l *remoteLink) servedCount() int {
	l.served.mu.Lock()
	defer l.served.mu.Unlock()
	return len(l.served.served)
}

// TestAttachFetchesLazily attaches a checkout from one registry into another over a
// pipe: listing and stat move no content, the first read fetches one file (plus
// its small siblings), and cached content survives the link going away.
func TestAttachFetchesLazily(t *testing.T) {
	src := newFixture(t)
	dir := folder(t, map[string]string{"a/one.txt": "one", "a/two.txt": "two", "b/big.bin": string(make([]byte, 300<<10)), ".env": "local", "node_modules/x.js": "x"})
	src.newCheckout(dir, "app")
	src.put("app", "a/new.txt", "written in the checkout")
	l := attachOverPipe(t, src, "app")
	reg, counter, got := l.reg, l.served, l.offer
	dst := &fixture{t: t, reg: reg}
	h := api.Checkout{Name: l.name}

	view, err := reg.Enter(h.Name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := view.FS.Lstat("b/big.bin")
	view.Done()
	if err != nil || info.Size() != 300<<10 {
		t.Fatalf("stat big.bin = %v %v", info, err)
	}
	for _, p := range []string{".env", "node_modules/x.js"} {
		if got := dst.read(h.Name, p); got != "<absent>" {
			t.Errorf("%s crossed machines: %q", p, got)
		}
	}
	if len(counter.served) != 0 {
		t.Fatalf("metadata access moved content: %v", counter.served)
	}
	if got := dst.read(h.Name, "a/new.txt"); got != "written in the checkout" {
		t.Fatalf("a/new.txt = %q", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for l.servedCount() != 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := l.servedCount(); n != 3 {
		t.Fatalf("first read served %d blobs, want a/new.txt plus its two siblings", n)
	}

	l.stop()
	if got := dst.read(h.Name, "a/one.txt"); got != "one" {
		t.Fatalf("cached sibling after link loss = %q", got)
	}
	again, err := reg.AttachRemote(context.Background(), got)
	if err != nil || again.Name != h.Name {
		t.Fatalf("re-offering the same snapshot: %+v %v", again, err)
	}
	bad := got
	bad.Files = append([]peer.Entry{}, got.Files...)
	bad.Files[0].ID = store.HashBytes("blob", []byte("forged"))
	if _, err := reg.AttachRemote(context.Background(), bad); err == nil {
		t.Fatal("accepted an offer whose files do not hash to its tree")
	}
	if _, err := reg.Apply(context.Background(), api.ApplyArgs{Checkout: h.Name}); err == nil {
		t.Fatal("apply ran on a remote checkout")
	}
}
