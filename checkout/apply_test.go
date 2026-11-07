package checkout

import (
	"context"
	"errors"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// TestApplyDryRunKeepsNoSnapshot checks that a dry run reports the same changes
// as a real apply but leaves no snapshot behind to hold the checkout's layers.
func TestApplyDryRunKeepsNoSnapshot(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "a"})
	f.newCheckout(dir, "c")
	f.put("c", "a.txt", "edit")
	ctx := context.Background()

	dry, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c", DryRun: true})
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	if n := len(f.reg.Ls().Snapshots); n != 0 {
		t.Errorf("snapshots after a dry run = %d, want 0", n)
	}
	if dry.Snapshot.ID != "" {
		t.Errorf("dry run reported snapshot %q, want none", dry.Snapshot.ID)
	}
	if len(dry.Applied) != 1 || dry.Applied[0].Path != "a.txt" {
		t.Errorf("dry run applied = %+v, want a.txt", dry.Applied)
	}

	real, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(real.Applied) != 1 || real.Applied[0].Path != "a.txt" || real.Snapshot.ID == "" {
		t.Errorf("apply = %+v, want a.txt from a kept snapshot", real)
	}
	if n := len(f.reg.Ls().Snapshots); n != 1 {
		t.Errorf("snapshots after apply = %d, want 1", n)
	}
}

// TestApplyDryRunCanceledKeepsNoSnapshot checks that a dry run canceled while it
// waits for its snapshot's tree still leaves no snapshot behind.
func TestApplyDryRunCanceledKeepsNoSnapshot(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hashed := make(chan struct{})
	defer close(hashed)
	f.reg.config.Emit = func(e Event) {
		switch e.Name {
		case "snapshotted":
			cancel() // the snapshot exists; its tree is not known yet
		case "hashed":
			<-hashed // hold the tree back until the dry run has returned
		}
	}
	dir := folder(t, map[string]string{"a.txt": "a"})
	f.newCheckout(dir, "c")
	f.put("c", "a.txt", "edit")

	_, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c", DryRun: true})
	var public *Error
	if !errors.As(err, &public) || public.Code != "unavailable" {
		t.Fatalf("canceled apply --dry-run: %v", err)
	}
	if n := len(f.reg.Ls().Snapshots); n != 0 {
		t.Errorf("snapshots after a canceled dry run = %d, want 0", n)
	}
}
