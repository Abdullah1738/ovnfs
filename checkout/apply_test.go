package checkout

import (
	"context"
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
