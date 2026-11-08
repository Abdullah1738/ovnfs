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
	"github.com/stencil-hq/ovnfs/internal/hostfile"
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
	if n := len(f.ls().Snapshots); n != 0 {
		t.Errorf("snapshots after concurrent applies = %d, want 0", n)
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
	if len(dry.Applied) != 1 || dry.Applied[0].Path != "a.txt" {
		t.Errorf("dry run applied = %+v, want a.txt", dry.Applied)
	}

	real, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(real.Applied) != 1 || real.Applied[0].Path != "a.txt" {
		t.Errorf("apply = %+v, want a.txt", real)
	}
	if n := len(f.ls().Snapshots); n != 0 {
		t.Errorf("snapshots after apply = %d, want 0", n)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(data) != "edit" {
		t.Errorf("source after apply = %q, %v; want edit", data, err)
	}
}

// TestApplyReplacesHeldAndReadOnlyFiles checks that apply overwrites folder
// files as a Unix rename does, where Windows' plain rename refuses a read-only
// file or one another process holds open sharing delete access.
func TestApplyReplacesHeldAndReadOnlyFiles(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"held.txt": "old", "readonly.txt": "old"})
	f.newCheckout(dir, "c")
	f.put("c", "held.txt", "new")
	f.put("c", "readonly.txt", "new")
	if err := os.Chmod(filepath.Join(dir, "readonly.txt"), 0o444); err != nil {
		t.Fatal(err)
	}
	held, err := hostfile.Open(filepath.Join(dir, "held.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if _, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "c"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, name := range []string{"held.txt", "readonly.txt"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != "new" {
			t.Errorf("%s after apply = %q, %v; want new", name, data, err)
		}
	}
}

// TestApplyPreservesOtherSnapshots verifies the transient snapshot is the only
// one released, and previously created snapshots remain usable.
func TestApplyPreservesOtherSnapshots(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "base"})
	f.newCheckout(dir, "c")
	f.put("c", "a.txt", "first")
	ctx := context.Background()
	snap, err := f.reg.Snap(ctx, api.SnapArgs{Checkout: "c", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	f.put("c", "a.txt", "second")
	if _, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c"}); err != nil {
		t.Fatal(err)
	}
	snapshots := f.ls().Snapshots
	if len(snapshots) != 1 || snapshots[0].ID != snap.Snapshot.ID {
		t.Fatalf("snapshots after apply = %+v, want only %s", snapshots, snap.Snapshot.ID)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Name: "view", Snapshot: snap.Snapshot.ID}); err != nil {
		t.Fatal(err)
	}
	if got := f.read("view", "a.txt"); got != "first" {
		t.Errorf("snapshot content = %q, want first", got)
	}
	if got := f.read("c", "a.txt"); got != "second" {
		t.Errorf("checkout content = %q, want second", got)
	}
}

// TestApplyConflictKeepsSnapshot checks that an unresolved apply retains its
// snapshot even though conflicts are reported in the reply rather than an error.
func TestApplyConflictKeepsSnapshot(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "base\x00"})
	f.newCheckout(dir, "c")
	f.put("c", "a.txt", "checkout\x00")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("source\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	reply, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Conflicts) != 1 || reply.Conflicts[0].Path != "a.txt" {
		t.Fatalf("apply conflicts = %+v, want a.txt", reply.Conflicts)
	}
	snapshots := f.ls().Snapshots
	if len(snapshots) != 1 {
		t.Fatalf("snapshots after conflicted apply = %+v, want one", snapshots)
	}
	if _, err := f.reg.New(context.Background(), api.NewArgs{Name: "view", Snapshot: snapshots[0].ID}); err != nil {
		t.Fatal(err)
	}
	if got := f.read("view", "a.txt"); got != "checkout\x00" {
		t.Errorf("snapshot content = %q, want checkout", got)
	}
}

// TestApplyFailureKeepsSnapshot checks that a failure after hashing preserves
// the apply snapshot for inspection.
func TestApplyFailureKeepsSnapshot(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "base"})
	f.newCheckout(dir, "c")
	f.put("c", "a.txt", "edit")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "c"}); err == nil {
		t.Fatal("apply with invalid source succeeded")
	}
	snapshots := f.ls().Snapshots
	if len(snapshots) != 1 {
		t.Fatalf("snapshots after failed apply = %+v, want one", snapshots)
	}
	if _, err := f.reg.New(context.Background(), api.NewArgs{Name: "view", Snapshot: snapshots[0].ID}); err != nil {
		t.Fatal(err)
	}
	if got := f.read("view", "a.txt"); got != "edit" {
		t.Errorf("snapshot content = %q, want edit", got)
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
