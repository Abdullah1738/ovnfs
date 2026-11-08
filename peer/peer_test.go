package peer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// memBlobs serves blobs from memory, counting opens; hooks shape individual blobs.
type memBlobs struct {
	mu    sync.Mutex
	data  map[store.OID][]byte
	serve map[store.OID][]byte // bytes served instead of data (corruption)
	fail  map[store.OID]string // Open error
	gate  map[store.OID]chan struct{}
	stall map[store.OID]chan struct{} // reader blocks after its first chunk until closed
	opens map[store.OID]int
	paths map[store.OID]string
	open  chan store.OID // receives every opened id when non-nil
}

// newBlobs returns an empty in-memory source.
func newBlobs() *memBlobs {
	return &memBlobs{
		data:  map[store.OID][]byte{},
		serve: map[store.OID][]byte{},
		fail:  map[store.OID]string{},
		gate:  map[store.OID]chan struct{}{},
		stall: map[store.OID]chan struct{}{},
		opens: map[store.OID]int{},
		paths: map[store.OID]string{},
	}
}

// add stores data and returns its blob id.
func (b *memBlobs) add(data []byte) store.OID {
	id := store.HashBytes("blob", data)
	b.mu.Lock()
	b.data[id] = data
	b.mu.Unlock()
	return id
}

// Open serves id's bytes after applying the blob's hooks.
func (b *memBlobs) Open(id store.OID, path string) (io.ReadCloser, int64, error) {
	b.mu.Lock()
	b.opens[id]++
	b.paths[id] = path
	gate, stall, msg, open := b.gate[id], b.stall[id], b.fail[id], b.open
	data, ok := b.serve[id]
	if !ok {
		data, ok = b.data[id]
	}
	b.mu.Unlock()
	if open != nil {
		open <- id
	}
	if gate != nil {
		<-gate
	}
	if msg != "" {
		return nil, 0, errors.New(msg)
	}
	if !ok {
		return nil, 0, fmt.Errorf("object %s not found", id)
	}
	var r io.Reader = bytes.NewReader(data)
	if stall != nil {
		r = io.MultiReader(bytes.NewReader(data[:chunkSize]), &stalled{stall, bytes.NewReader(data[chunkSize:])})
	}
	return io.NopCloser(r), int64(len(data)), nil
}

// hold makes Open of id wait until the returned release is called (callers defer it so a failing test never strands Serve).
func (b *memBlobs) hold(id store.OID) (release func()) {
	gate := make(chan struct{})
	b.gate[id] = gate
	return sync.OnceFunc(func() { close(gate) })
}

// opened reports how many times the source opened id.
func (b *memBlobs) opened(id store.OID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.opens[id]
}

// stalled blocks every read until gate is closed, then reads slowly.
type stalled struct {
	gate chan struct{}
	r    io.Reader
}

// Read waits for the gate, then returns at most 32 KiB.
func (s *stalled) Read(p []byte) (int, error) {
	<-s.gate
	time.Sleep(time.Millisecond)
	return s.r.Read(p[:min(len(p), 32<<10)])
}

// link is a running source and accepted remote session.
type link struct {
	s      *Session
	remote *Remote
	st     *store.Store
	dir    string
	served chan error
	cancel context.CancelFunc
}

// start runs Serve on src and an accepted, replied Session on dst.
func start(t *testing.T, src, dst io.ReadWriteCloser, blobs Blobs) *link {
	t.Helper()
	return startWith(t, src, dst, blobs, nil)
}

// startPull prepares a link with remote snapshot handling before acceptance.
func startPull(t *testing.T, src, dst io.ReadWriteCloser, blobs Blobs, handler func(context.Context, store.OID) (Pulled, error)) *link {
	t.Helper()
	return startWith(t, src, dst, blobs, func(s *Session) { s.SetPullHandler(handler) })
}

// startPush prepares a link over net.Pipe whose pushes fetch what list returns.
func startPush(t *testing.T, blobs Blobs, list func() []Want) *link {
	t.Helper()
	src, dst := net.Pipe()
	return startWith(t, src, dst, blobs, func(s *Session) { s.SetPushHandler(list) })
}

// startWith runs ServeRemote on src and, on dst, a Session that setup (if any)
// configures before it is accepted.
func startWith(t *testing.T, src, dst io.ReadWriteCloser, blobs Blobs, setup func(*Session)) *link {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &link{st: st, dir: dir, served: make(chan error, 1), cancel: cancel}
	ready := make(chan *Remote, 1)
	go func() {
		l.served <- ServeRemote(ctx, src, offerOf(3), blobs, func(_ Accepted, p *Remote) { ready <- p })
	}()
	s, _, err := Accept(dst, st)
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(s)
	}
	if err := s.Reply(Accepted{Checkout: "h", Path: "/checkouts/h", Host: "b"}); err != nil {
		t.Fatal(err)
	}
	l.s = s
	l.remote = wait(t, ready, "remote handle")
	t.Cleanup(func() {
		s.Close()
		cancel()
		<-l.served
	})
	return l
}

// pipeLink is start over net.Pipe.
func pipeLink(t *testing.T, blobs Blobs) *link {
	a, b := net.Pipe()
	return start(t, a, b, blobs)
}

// offerOf builds a valid offer with n file entries.
func offerOf(n int) Offer {
	files := make([]Entry, n)
	for i := range files {
		data := fmt.Appendf(nil, "file %d", i)
		files[i] = Entry{
			Path:  fmt.Sprintf("dir%03d/file%05d.go", i/1000, i),
			Mode:  store.ModeFile,
			ID:    store.HashBytes("blob", data),
			Size:  int64(len(data)),
			Mtime: int64(i) * 1e9,
		}
	}
	files = append(files, Entry{Path: "zz/empty", Mode: store.ModeDir, ID: store.EmptyTree})
	return Offer{
		Node:   "node-a",
		Source: "laptop:/src/app",
		Name:   "app",
		Tree:   store.HashBytes("tree", nil),
		Lock: manifest.Lock{
			Rules: []manifest.LockRule{{Class: rules.Cache, Text: "node_modules/", Origin: "builtin"}},
			Env:   manifest.Env{Image: "alpine"},
		},
		Files:   files,
		Roots:   map[string]rules.Class{"": rules.Source, "node_modules": rules.Cache},
		Created: time.Unix(1_700_000_000, 123).UTC(),
	}
}

// wait returns the value from ch or fails after a generous timeout.
func wait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// fetchAsync runs Fetch in the background.
func fetchAsync(s *Session, ctx context.Context, wants ...Want) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- s.Fetch(ctx, wants) }()
	return ch
}

// checkBlob fails unless st holds exactly data under id.
func checkBlob(t *testing.T, st *store.Store, id store.OID, data []byte) {
	t.Helper()
	got, err := st.ReadBlob(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("blob %s: got %d bytes, want %d", id, len(got), len(data))
	}
}

// randomBytes returns n reproducible pseudo-random bytes.
func randomBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestPullRoundTripDuringFetch(t *testing.T) {
	blobs := newBlobs()
	fetched := randomBytes(3<<20, 41)
	fetchID := blobs.add(fetched)
	before := []byte("before")
	after := randomBytes(3<<20+17, 42)
	oldID := store.HashBytes("blob", before)
	newID := store.HashBytes("blob", after)
	emptyID := store.HashBytes("blob", nil)
	want := Pulled{
		Snapshot: api.Snapshot{ID: "remote-snap", Checkout: "remote", Tree: string(offerOf(0).Tree)},
		Base: map[string]store.File{
			"changed": {Mode: store.ModeFile, ID: oldID},
			"gone":    {Mode: store.ModeFile, ID: oldID},
		},
		Files: map[string]store.File{
			"changed": {Mode: store.ModeExec, ID: newID},
			"empty":   {Mode: store.ModeFile, ID: emptyID},
			"dir":     {Mode: store.ModeDir, ID: store.EmptyTree},
		},
		Data: map[store.OID][]byte{oldID: before, newID: after, emptyID: nil},
	}
	began := make(chan store.OID, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	src, dst := net.Pipe()
	l := startPull(t, src, dst, blobs, func(ctx context.Context, from store.OID) (Pulled, error) {
		began <- from
		select {
		case <-release:
			return want, nil
		case <-ctx.Done():
			return Pulled{}, ctx.Err()
		}
	})
	type result struct {
		p   Pulled
		err error
	}
	pending := make(chan result, 1)
	go func() {
		p, err := l.remote.Pull(context.Background(), offerOf(3).Tree)
		pending <- result{p, err}
	}()
	if from := wait(t, began, "remote snapshot"); from != offerOf(3).Tree {
		t.Fatalf("snapshot base %s", from)
	}
	// A blocked snapshot callback must not block requests in either direction.
	if err := wait(t, fetchAsync(l.s, context.Background(), Want{ID: fetchID}), "fetch during snapshot"); err != nil {
		t.Fatal(err)
	}
	checkBlob(t, l.st, fetchID, fetched)
	unblock()
	second := randomBytes(2<<20, 43)
	secondID := blobs.add(second)
	fetching := fetchAsync(l.s, context.Background(), Want{ID: secondID})
	got := wait(t, pending, "streamed pull")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if !reflect.DeepEqual(got.p, want) {
		t.Fatal("pull metadata or blob bytes changed in transit")
	}
	if err := wait(t, fetching, "fetch alongside pull data"); err != nil {
		t.Fatal(err)
	}
	checkBlob(t, l.st, secondID, second)
}

func TestPullSnapshotFailureLeavesFetchWorking(t *testing.T) {
	for _, handler := range []func(context.Context, store.OID) (Pulled, error){
		nil,
		func(context.Context, store.OID) (Pulled, error) {
			return Pulled{}, errors.New("cannot snapshot changed checkout")
		},
	} {
		src, dst := net.Pipe()
		blobs := newBlobs()
		id := blobs.add([]byte("still available"))
		l := startPull(t, src, dst, blobs, handler)
		for range 2 {
			_, err := l.remote.Pull(context.Background(), offerOf(3).Tree)
			if err == nil || !strings.Contains(err.Error(), "remote snapshot:") {
				t.Fatalf("pull error = %v", err)
			}
			if handler != nil && !strings.Contains(err.Error(), "cannot snapshot changed checkout") {
				t.Fatalf("snapshot failure not preserved: %v", err)
			}
		}
		if err := l.s.Fetch(context.Background(), []Want{{ID: id}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPendingPullReleasedOnCancellationAndShutdown(t *testing.T) {
	for _, mode := range []string{"request canceled", "remote closed", "source canceled", "link lost"} {
		t.Run(mode, func(t *testing.T) {
			began := make(chan struct{})
			released := make(chan struct{})
			src, dst := net.Pipe()
			l := startPull(t, src, dst, newBlobs(), func(ctx context.Context, _ store.OID) (Pulled, error) {
				close(began)
				<-ctx.Done()
				close(released)
				return Pulled{}, ctx.Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pending := make(chan error, 1)
			go func() {
				_, err := l.remote.Pull(ctx, offerOf(3).Tree)
				pending <- err
			}()
			wait(t, began, "snapshot started")
			switch mode {
			case "request canceled":
				cancel()
			case "remote closed":
				l.s.Close()
			case "source canceled":
				l.cancel()
			case "link lost":
				dst.Close()
			}
			err := wait(t, pending, "pending pull")
			if err == nil {
				t.Fatal("pending pull succeeded")
			}
			if mode == "request canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("pull = %v, want canceled", err)
			}
			wait(t, released, "snapshot cancellation")
			if mode == "request canceled" {
				id := store.HashBytes("blob", []byte("absent"))
				err := l.s.Fetch(context.Background(), []Want{{ID: id}})
				if err == nil || !strings.Contains(err.Error(), "not found") {
					t.Fatalf("link unusable after canceled pull: %v", err)
				}
			}
		})
	}
}

func TestPullValidatesBaseAndContext(t *testing.T) {
	src, dst := net.Pipe()
	l := startPull(t, src, dst, newBlobs(), nil)
	if _, err := l.remote.Pull(context.Background(), "invalid"); err == nil {
		t.Fatal("invalid base accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.remote.Pull(ctx, offerOf(3).Tree); !errors.Is(err, context.Canceled) {
		t.Fatalf("pull with canceled context: %v", err)
	}
}

func TestPullRejectsCorruptBlob(t *testing.T) {
	src, dst := net.Pipe()
	id := store.HashBytes("blob", []byte("wanted"))
	l := startPull(t, src, dst, newBlobs(), func(context.Context, store.OID) (Pulled, error) {
		return Pulled{Data: map[store.OID][]byte{id: []byte("wrong")}}, nil
	})
	pending := make(chan error, 1)
	go func() {
		_, err := l.remote.Pull(context.Background(), offerOf(3).Tree)
		pending <- err
	}()
	if err := wait(t, pending, "corrupt pull"); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("corrupt pull = %v", err)
	}
}

// pushWants lists blobs in order as a push handler would, each under its own path.
func pushWants(blobs *memBlobs, ids ...store.OID) []Want {
	wants := make([]Want, len(ids))
	for i, id := range ids {
		wants[i] = Want{ID: id, Path: fmt.Sprintf("f%d", i), Size: int64(len(blobs.data[id]))}
	}
	return wants
}

// TestPushFetchesInBatches: a push fetches everything listed and reports it,
// but asks the source for the next batch only once the previous one landed.
func TestPushFetchesInBatches(t *testing.T) {
	blobs := newBlobs()
	var ids []store.OID
	for i := range 3 {
		ids = append(ids, blobs.add(randomBytes(3<<20, uint64(i+1)))) // two fit a batch, the third does not
	}
	ids = append(ids, blobs.add([]byte("small")), blobs.add(nil))
	l := startPush(t, blobs, func() []Want { return pushWants(blobs, ids...) })
	blobs.open = make(chan store.OID, len(ids))
	release := blobs.hold(ids[0])
	defer release()
	pushed := make(chan error, 1)
	var got Pushed
	go func() {
		var err error
		got, err = l.remote.Push(context.Background())
		pushed <- err
	}()
	// The source opens both blobs of the first batch, the first of them held.
	for opened := map[store.OID]bool{}; !opened[ids[1]]; {
		opened[wait(t, blobs.open, "first batch")] = true
	}
	time.Sleep(50 * time.Millisecond)
	for _, id := range ids[2:] {
		if blobs.opened(id) != 0 {
			t.Fatalf("blob %s of the second batch requested before the first landed", id)
		}
	}
	release()
	if err := wait(t, pushed, "push"); err != nil {
		t.Fatal(err)
	}
	want := Pushed{Blobs: int64(len(ids)), Bytes: 3*(3<<20) + int64(len("small"))}
	if got != want {
		t.Fatalf("pushed %+v, want %+v", got, want)
	}
	for _, id := range ids {
		checkBlob(t, l.st, id, blobs.data[id])
	}
}

// TestPushCancelStopsLaterBatches: canceling a push on the source stops the
// remote before it asks for the next batch.
func TestPushCancelStopsLaterBatches(t *testing.T) {
	blobs := newBlobs()
	first := blobs.add(randomBytes(5<<20, 1))
	second := blobs.add(randomBytes(5<<20, 2))
	l := startPush(t, blobs, func() []Want { return pushWants(blobs, first, second) })
	blobs.open = make(chan store.OID, 2)
	release := blobs.hold(first)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	pushed := make(chan error, 1)
	go func() {
		_, err := l.remote.Push(ctx)
		pushed <- err
	}()
	wait(t, blobs.open, "first batch")
	cancel()
	if err := wait(t, pushed, "canceled push"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled push = %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		l.s.mu.Lock()
		running := len(l.s.pushes)
		l.s.mu.Unlock()
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the remote kept pushing after the source canceled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	time.Sleep(50 * time.Millisecond)
	if blobs.opened(second) != 0 {
		t.Fatal("a canceled push requested its next batch")
	}
}

// TestPushFailures: a remote without a push handler refuses, a blob the source
// cannot produce fails the push by name, and the link outlives both.
func TestPushFailures(t *testing.T) {
	blobs := newBlobs()
	if _, err := pipeLink(t, blobs).remote.Push(context.Background()); err == nil || !strings.Contains(err.Error(), "does not take pushes") {
		t.Fatalf("push without a handler = %v", err)
	}
	gone := blobs.add([]byte("gone"))
	blobs.fail[gone] = "object vanished"
	l := startPush(t, blobs, func() []Want { return pushWants(blobs, gone) })
	if _, err := l.remote.Push(context.Background()); err == nil || !strings.Contains(err.Error(), "object vanished") || !strings.Contains(err.Error(), "f0") {
		t.Fatalf("push of an unavailable blob = %v", err)
	}
	ok := blobs.add([]byte("still served"))
	if err := l.s.Fetch(context.Background(), []Want{{ID: ok}}); err != nil {
		t.Fatal(err)
	}
}

func TestOldPeersAreRejectedBeforePull(t *testing.T) {
	t.Run("old source", func(t *testing.T) {
		src, dst := net.Pipe()
		defer src.Close()
		st, err := store.Open(t.TempDir(), 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		accepted := make(chan error, 1)
		go func() {
			_, _, err := Accept(dst, st)
			accepted <- err
		}()
		if _, err := src.Write(frame(frameOffer, binary.BigEndian.AppendUint32([]byte("OVNL"), Version-1))); err != nil {
			t.Fatal(err)
		}
		if _, _, err := newFrameReader(src).next(frameAnswer); err != nil {
			t.Fatal(err)
		}
		if err := wait(t, accepted, "old source refusal"); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("version %d", Version-1)) {
			t.Fatalf("old source = %v", err)
		}
	})
	t.Run("old remote", func(t *testing.T) {
		src, dst := net.Pipe()
		defer dst.Close()
		served := make(chan error, 1)
		go func() {
			served <- Serve(context.Background(), src, offerOf(0), newBlobs(), nil)
		}()
		if _, _, err := newFrameReader(dst).next(frameOffer); err != nil {
			t.Fatal(err)
		}
		if _, err := dst.Write(frame(frameAnswer, binary.BigEndian.AppendUint32([]byte("OVNL"), Version-1), []byte(`{"accepted":{}}`))); err != nil {
			t.Fatal(err)
		}
		if err := wait(t, served, "old remote refusal"); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("version %d", Version-1)) {
			t.Fatalf("old remote = %v", err)
		}
	})
}

func TestOfferRoundTrip(t *testing.T) {
	src, dst := net.Pipe()
	want := offerOf(50_000)
	accepted := make(chan Accepted, 2)
	served := make(chan error, 1)
	go func() {
		served <- Serve(context.Background(), src, want, newBlobs(), func(a Accepted) { accepted <- a })
	}()
	st, err := store.Open(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	s, got, err := Accept(dst, st)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created.Equal(want.Created) {
		t.Fatalf("created %v, want %v", got.Created, want.Created)
	}
	got.Created, want.Created = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("offer changed in transit")
	}
	reply := Accepted{Checkout: "app", Path: "/checkouts/app", Host: "linux-box"}
	if err := s.Reply(reply); err != nil {
		t.Fatal(err)
	}
	if a := wait(t, accepted, "accepted"); a != reply {
		t.Fatalf("accepted %+v, want %+v", a, reply)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, served, "Serve"); err != nil {
		t.Fatalf("Serve after clean close: %v", err)
	}
	if len(accepted) != 0 {
		t.Fatal("accepted called more than once")
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err after Close: %v", err)
	}
}

func TestRefuse(t *testing.T) {
	src, dst := net.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- Serve(context.Background(), src, offerOf(1), newBlobs(), func(Accepted) { t.Error("accepted called on refusal") })
	}()
	st, _ := store.Open(t.TempDir(), 64<<20)
	s, _, err := Accept(dst, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Refuse(errors.New("checkout app already exists")); err != nil {
		t.Fatal(err)
	}
	err = wait(t, served, "Serve")
	if err == nil || !strings.Contains(err.Error(), "checkout app already exists") {
		t.Fatalf("Serve = %v, want the refusal message", err)
	}
	wait(t, s.Done(), "Done")
	if err := s.Fetch(context.Background(), []Want{{ID: store.HashBytes("blob", []byte("x"))}}); err == nil {
		t.Fatal("Fetch on a refused link succeeded")
	}
}

func TestVersionMismatchNamesBothVersions(t *testing.T) {
	t.Run("source newer", func(t *testing.T) {
		src, dst := net.Pipe()
		accepted := make(chan error, 1)
		go func() {
			st, _ := store.Open(t.TempDir(), 64<<20)
			_, _, err := Accept(dst, st)
			accepted <- err
		}()
		payload := binary.BigEndian.AppendUint32([]byte("OVNL"), Version+1)
		if _, err := src.Write(frame(frameOffer, payload, []byte("future format"))); err != nil {
			t.Fatal(err)
		}
		_, p, err := newFrameReader(src).next(frameAnswer)
		if err != nil {
			t.Fatal(err)
		}
		var ans answer
		body, err := checkHello(p, "remote")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &ans); err != nil {
			t.Fatal(err)
		}
		for _, msg := range []string{ans.Error, wait(t, accepted, "Accept").Error()} {
			if !strings.Contains(msg, fmt.Sprintf("version %d", Version+1)) || !strings.Contains(msg, fmt.Sprintf("version %d", Version)) {
				t.Fatalf("message %q does not name both versions", msg)
			}
		}
	})
	t.Run("remote newer", func(t *testing.T) {
		src, dst := net.Pipe()
		served := make(chan error, 1)
		go func() { served <- Serve(context.Background(), src, offerOf(1), newBlobs(), func(Accepted) {}) }()
		if _, _, err := newFrameReader(dst).next(frameOffer); err != nil {
			t.Fatal(err)
		}
		payload := binary.BigEndian.AppendUint32([]byte("OVNL"), 7)
		if _, err := dst.Write(frame(frameAnswer, payload, []byte(`{"accepted":{}}`))); err != nil {
			t.Fatal(err)
		}
		err := wait(t, served, "Serve")
		if err == nil || !strings.Contains(err.Error(), "version 7") || !strings.Contains(err.Error(), fmt.Sprintf("version %d", Version)) {
			t.Fatalf("Serve = %v, want both versions named", err)
		}
	})
}

func TestAcceptRejectsForeignAndOversizeStreams(t *testing.T) {
	for name, header := range map[string][]byte{
		"banner":   []byte("SSH-2.0-OpenSSH_9.6\r\n"),
		"oversize": {frameOffer, 0x04, 0x10, 0x00, 0x00}, // 65 MiB offer
	} {
		t.Run(name, func(t *testing.T) {
			src, dst := net.Pipe()
			go src.Write(header)
			st, _ := store.Open(t.TempDir(), 64<<20)
			_, _, err := Accept(dst, st)
			if err == nil {
				t.Fatal("Accept succeeded")
			}
			t.Log(err)
			src.Close()
		})
	}
}

func TestFetchSmallEmptyAndLarge(t *testing.T) {
	blobs := newBlobs()
	small := []byte("package main\n")
	large := randomBytes(3<<20+17, 1)
	ids := map[store.OID][]byte{
		blobs.add(small): small,
		blobs.add(nil):   nil,
		blobs.add(large): large,
	}
	l := pipeLink(t, blobs)
	var wants []Want
	for id, data := range ids {
		wants = append(wants, Want{ID: id, Path: fmt.Sprintf("f%d", len(data)), Size: int64(len(data))})
	}
	if err := l.s.Fetch(context.Background(), wants); err != nil {
		t.Fatal(err)
	}
	for id, data := range ids {
		checkBlob(t, l.st, id, data)
	}
	blobs.mu.Lock()
	hint := blobs.paths[store.HashBytes("blob", large)]
	blobs.mu.Unlock()
	if got := hint; got != fmt.Sprintf("f%d", len(large)) {
		t.Fatalf("source got path hint %q", got)
	}
	// Present blobs are not requested again.
	if err := l.s.Fetch(context.Background(), wants); err != nil {
		t.Fatal(err)
	}
	for id := range ids {
		if n := blobs.opened(id); n != 1 {
			t.Fatalf("blob %s opened %d times", id, n)
		}
	}
}

func TestFetchManyConcurrentRequestsEachBlobOnce(t *testing.T) {
	blobs := newBlobs()
	var wants []Want
	for i := range 600 {
		data := randomBytes(i*97%70_000, uint64(i))
		data = append(data, byte(i), byte(i>>8)) // distinct even when empty
		wants = append(wants, Want{ID: blobs.add(data), Path: fmt.Sprintf("f%d", i)})
	}
	l := pipeLink(t, blobs)
	var wg sync.WaitGroup
	errs := make(chan error, 9)
	// One prefetch-style batch of everything, plus overlapping slices from readers.
	wg.Go(func() { errs <- l.s.Fetch(context.Background(), wants) })
	for g := range 8 {
		wg.Go(func() {
			for i := g; i < len(wants); i += 8 {
				if err := l.s.Fetch(context.Background(), wants[i:i+1]); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range wants {
		checkBlob(t, l.st, w.ID, blobs.data[w.ID])
		if n := blobs.opened(w.ID); n != 1 {
			t.Fatalf("%s opened %d times", w.Path, n)
		}
	}
}

func TestConcurrentFetchOfSameBlobOpensSourceOnce(t *testing.T) {
	blobs := newBlobs()
	data := []byte("shared")
	id := blobs.add(data)
	release := blobs.hold(id)
	defer release()
	blobs.open = make(chan store.OID, 4)
	l := pipeLink(t, blobs)
	first := fetchAsync(l.s, context.Background(), Want{ID: id, Path: "a"})
	wait(t, blobs.open, "first open")
	second := fetchAsync(l.s, context.Background(), Want{ID: id, Path: "b"}, Want{ID: id, Path: "c"})
	time.Sleep(50 * time.Millisecond) // let the second Fetch join the transfer in flight
	release()
	for _, ch := range []<-chan error{first, second} {
		if err := wait(t, ch, "Fetch"); err != nil {
			t.Fatal(err)
		}
	}
	if n := blobs.opened(id); n != 1 {
		t.Fatalf("source opened the blob %d times", n)
	}
	checkBlob(t, l.st, id, data)
}

func TestSmallBlobOvertakesLargeOne(t *testing.T) {
	blobs := newBlobs()
	large := randomBytes(3<<20, 2)
	small := []byte("small")
	largeID, smallID := blobs.add(large), blobs.add(small)
	release := make(chan struct{})
	blobs.stall[largeID] = release
	blobs.open = make(chan store.OID, 4)
	l := pipeLink(t, blobs)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	bigDone := fetchAsync(l.s, context.Background(), Want{ID: largeID, Path: "big"})
	wait(t, blobs.open, "large open")
	if err := wait(t, fetchAsync(l.s, context.Background(), Want{ID: smallID, Path: "small"}), "small Fetch"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-bigDone:
		t.Fatalf("large blob finished before it was released: %v", err)
	default:
	}
	close(release)
	if err := wait(t, bigDone, "large Fetch"); err != nil {
		t.Fatal(err)
	}
	checkBlob(t, l.st, largeID, large)
	checkBlob(t, l.st, smallID, small)
}

func TestCorruptBlobIsRejectedAndNotStored(t *testing.T) {
	blobs := newBlobs()
	sameSize := blobs.add([]byte("good!"))
	blobs.serve[sameSize] = []byte("evil!")
	otherSize := blobs.add([]byte("tiny"))
	blobs.serve[otherSize] = randomBytes(600<<10, 3) // announced size differs and spans chunks
	l := pipeLink(t, blobs)
	for _, id := range []store.OID{sameSize, otherSize} {
		err := l.s.Fetch(context.Background(), []Want{{ID: id, Path: "f", Size: 5}})
		if err == nil || !strings.Contains(err.Error(), "hashes to") {
			t.Fatalf("Fetch of corrupt blob = %v", err)
		}
		if l.st.Has(id) {
			t.Fatal("corrupt blob was stored")
		}
	}
	if left, _ := os.ReadDir(filepath.Join(l.dir, "tmp")); len(left) != 0 {
		t.Fatalf("staging files left behind: %v", left)
	}
	// The link survives a corrupt blob.
	good := blobs.add([]byte("fine"))
	if err := l.s.Fetch(context.Background(), []Want{{ID: good}}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingObjectFailsOnlyItsWaiters(t *testing.T) {
	blobs := newBlobs()
	missing := store.HashBytes("blob", []byte("gone"))
	blobs.fail[missing] = "object gone not in repository /src/app"
	slow := []byte("slow but fine")
	slowID := blobs.add(slow)
	release := blobs.hold(slowID)
	defer release()
	other := []byte("batched with the missing one")
	otherID := blobs.add(other)
	l := pipeLink(t, blobs)

	okDone := fetchAsync(l.s, context.Background(), Want{ID: slowID, Path: "slow"})
	err := l.s.Fetch(context.Background(), []Want{{ID: otherID, Path: "other"}, {ID: missing, Path: "gone"}})
	if err == nil || !strings.Contains(err.Error(), "object gone not in repository /src/app") {
		t.Fatalf("Fetch of missing blob = %v, want the source's message", err)
	}
	release()
	if err := wait(t, okDone, "unrelated Fetch"); err != nil {
		t.Fatalf("unrelated Fetch failed: %v", err)
	}
	checkBlob(t, l.st, slowID, slow)
	checkBlob(t, l.st, otherID, other)
	// A later Fetch asks again (the failure is not cached) and the link is still up.
	if err := l.s.Fetch(context.Background(), []Want{{ID: missing}}); err == nil {
		t.Fatal("second Fetch of missing blob succeeded")
	}
	if n := blobs.opened(missing); n != 2 {
		t.Fatalf("missing blob opened %d times, want 2", n)
	}
}

func TestLinkLossFailsPendingFetches(t *testing.T) {
	blobs := newBlobs()
	id := blobs.add(randomBytes(1<<20, 4))
	defer blobs.hold(id)()
	blobs.open = make(chan store.OID, 1)
	src, dst := net.Pipe()
	l := start(t, src, dst, blobs)
	pending := fetchAsync(l.s, context.Background(), Want{ID: id})
	wait(t, blobs.open, "open")
	src.Close() // the source machine vanishes
	if err := wait(t, pending, "pending Fetch"); err == nil {
		t.Fatal("pending Fetch succeeded after link loss")
	}
	wait(t, l.s.Done(), "Done")
	if l.s.Err() == nil {
		t.Fatal("Err is nil after the stream broke")
	}
	if err := l.s.Fetch(context.Background(), []Want{{ID: id}}); err == nil {
		t.Fatal("Fetch after link loss succeeded")
	}
}

func TestCloseFailsPendingFetchesCleanly(t *testing.T) {
	blobs := newBlobs()
	id := blobs.add([]byte("never sent"))
	release := blobs.hold(id)
	defer release()
	blobs.open = make(chan store.OID, 1)
	l := pipeLink(t, blobs)
	pending := fetchAsync(l.s, context.Background(), Want{ID: id})
	wait(t, blobs.open, "open")
	l.s.Close()
	if err := wait(t, pending, "pending Fetch"); err == nil {
		t.Fatal("pending Fetch succeeded after Close")
	}
	if err := l.s.Err(); err != nil {
		t.Fatalf("Err after Close = %v", err)
	}
	release()
	if err := wait(t, l.served, "Serve"); err != nil {
		t.Fatalf("Serve after remote Close = %v", err)
	}
	l.served <- nil // for cleanup
}

func TestServeCancelSaysGoodbye(t *testing.T) {
	blobs := newBlobs()
	l := pipeLink(t, blobs)
	l.cancel()
	err := wait(t, l.served, "Serve")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v, want context.Canceled", err)
	}
	l.served <- err
	wait(t, l.s.Done(), "Done")
	if err := l.s.Err(); err != nil {
		t.Fatalf("Err after the source said goodbye = %v", err)
	}
}

func TestFetchHonorsContext(t *testing.T) {
	blobs := newBlobs()
	data := []byte("late")
	id := blobs.add(data)
	release := blobs.hold(id)
	defer release()
	l := pipeLink(t, blobs)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	err := l.s.Fetch(ctx, []Want{{ID: id}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch = %v, want deadline exceeded", err)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("Fetch took %v after its deadline", d)
	}
	// The abandoned transfer still completes, and a new Fetch joins it.
	release()
	if err := l.s.Fetch(context.Background(), []Want{{ID: id}}); err != nil {
		t.Fatal(err)
	}
	checkBlob(t, l.st, id, data)
	if n := blobs.opened(id); n != 1 {
		t.Fatalf("blob opened %d times", n)
	}
}

func TestFetchBeforeReplyFails(t *testing.T) {
	src, dst := net.Pipe()
	go Serve(context.Background(), src, offerOf(1), newBlobs(), func(Accepted) {})
	st, _ := store.Open(t.TempDir(), 64<<20)
	s, _, err := Accept(dst, st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Fetch(context.Background(), []Want{{ID: store.HashBytes("blob", nil)}}); err == nil {
		t.Fatal("Fetch before Reply succeeded")
	}
}

func TestFetchOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conns := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		conns <- c
	}()
	dst, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	src := wait(t, conns, "tcp accept")

	blobs := newBlobs()
	var wants []Want
	for i := range 300 {
		wants = append(wants, Want{ID: blobs.add(randomBytes(i*311, uint64(100+i))), Path: fmt.Sprintf("s%d", i)})
	}
	for i := range 3 {
		wants = append(wants, Want{ID: blobs.add(randomBytes(3<<20, uint64(10+i))), Path: fmt.Sprintf("big%d", i)})
	}
	l := start(t, src, dst, blobs)
	var wg sync.WaitGroup
	errs := make(chan error, len(wants))
	for _, w := range wants {
		wg.Go(func() { errs <- l.s.Fetch(context.Background(), []Want{w}) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range wants {
		checkBlob(t, l.st, w.ID, blobs.data[w.ID])
	}
	l.s.Close()
	if err := wait(t, l.served, "Serve"); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	l.served <- nil
}
