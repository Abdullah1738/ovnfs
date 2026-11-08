package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// TestApplyConcurrentKeepsEveryEdit checks that checkouts of one folder that
// edit different lines of one file and apply at once all land: each apply
// merges against the folder as the others left it, not the same old content.
func TestApplyConcurrentKeepsEveryEdit(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	const n = 8
	lines := make([]string, n*10)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	dir := folder(t, map[string]string{"shared.txt": strings.Join(lines, "\n") + "\n"})
	for i := range n {
		edited := slices.Clone(lines)
		edited[10*i+5] = fmt.Sprintf("edit by %d", i)
		f.newCheckout(dir, fmt.Sprint("c", i))
		f.put(fmt.Sprint("c", i), "shared.txt", strings.Join(edited, "\n")+"\n")
	}

	var wg sync.WaitGroup
	replies := make([]api.ApplyReply, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			replies[i], errs[i] = f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: fmt.Sprint("c", i)})
		})
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil || len(replies[i].Conflicts) != 0 {
			t.Errorf("apply c%d = %+v, %v", i, replies[i], errs[i])
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if !strings.Contains(string(data), fmt.Sprintf("\nedit by %d\n", i)) {
			t.Errorf("edit by c%d lost; folder has:\n%s", i, data)
		}
	}
}

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
	if n := len(f.ls().Snapshots); n != 0 {
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
	if n := len(f.ls().Snapshots); n != 1 {
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
	if n := len(f.ls().Snapshots); n != 0 {
		t.Errorf("snapshots after a canceled dry run = %d, want 0", n)
	}
}
