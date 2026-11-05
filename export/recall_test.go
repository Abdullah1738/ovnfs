//go:build !windows

package export_test

import (
	"context"
	"encoding/binary"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/internal/nfstest"
)

// recorder collects the stable nodes the export asks the frontend to recall.
type recorder struct {
	mu  sync.Mutex
	ids []uint64
}

// add records one recalled node.
func (r *recorder) add(id uint64) {
	r.mu.Lock()
	r.ids = append(r.ids, id)
	r.mu.Unlock()
}

// has reports whether the node in fh was recalled, then clears the recorder.
func (r *recorder) has(fh []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var id uint64
	if len(fh) == 8 {
		id = binary.BigEndian.Uint64(fh)
	}
	found := slices.Contains(r.ids, id)
	r.ids = nil
	return found
}

// TestRecallsChangesBehindNFSClients covers every way a file changes without
// an NFS request: another protocol, the daemon (Invalidate), and checkout
// removal, while NFS requests leave recalls to the NFS server. Vanished paths retire
// their handles so clients see stale.
func TestRecallsChangesBehindNFSClients(t *testing.T) {
	f := newStack(t, map[string]string{"src": "base"})
	f.create(t, "app")
	var rec recorder
	f.export.OnRecall(rec.add)
	c := nfstest.Start(t, f.export.NFS())
	src := lookup(t, c, "app/src")
	gone := nfsCreate(t, c, lookup(t, c, "app"), "gone", "doomed")
	rec.has(nil) // the client's own requests are not the subject here

	nfsWrite(t, c, src, "nfs")
	if rec.has(src) {
		t.Fatal("an NFS write went through the export's recall hook; the NFS server recalls conflicting delegations itself")
	}

	other, err := f.export.Nodes().OpenFile("app/src", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if !rec.has(src) {
		t.Fatal("a write through another protocol did not recall the NFS handle")
	}

	s, err := f.reg.Enter("app")
	if err != nil {
		t.Fatal(err)
	}
	err = s.FS.Remove("gone")
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	f.export.Invalidate("app", []string{"gone", "never-handed-out"})
	if !rec.has(gone) {
		t.Fatal("Invalidate did not recall the changed path")
	}
	_, status, err := c.GetAttr(gone)
	nfsStatus(t, status, nfstest.Stale, err)

	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	if !rec.has(src) {
		t.Fatal("removing the checkout did not recall its handles")
	}
}
