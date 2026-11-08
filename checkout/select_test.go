package checkout

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// selectFixture opens app and app-edit (with a source change) on one folder and
// other on a second, all created three hours ago.
func selectFixture(t *testing.T) (f *fixture, app string) {
	t.Helper()
	f = newFixture(t)
	app = folder(t, map[string]string{"a": "base", "sub/b": "b"})
	f.newCheckout(app, "app")
	f.newCheckout(app, "app-edit")
	f.newCheckout(folder(t, map[string]string{"a": "other"}), "other")
	f.put("app-edit", "a", "edited")
	f.reg.mu.Lock()
	for _, h := range f.reg.checkouts {
		h.meta.Created = h.meta.Created.Add(-3 * time.Hour)
	}
	f.reg.mu.Unlock()
	return f, app
}

// picked lists the names sel picks.
func (f *fixture) picked(sel api.Select) []string {
	f.t.Helper()
	reply, err := f.reg.Ls(context.Background(), api.LsArgs{Select: sel})
	if err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for _, c := range reply.Checkouts {
		names = append(names, c.Name)
	}
	return names
}

// TestSelect checks each criterion and that given together they all must hold.
func TestSelect(t *testing.T) {
	f, app := selectFixture(t)
	s, err := f.reg.Enter("other")
	if err != nil {
		t.Fatal(err)
	}
	s.Touch()
	s.Done()
	for _, tt := range []struct {
		name string
		sel  api.Select
		want []string
	}{
		{"folder", api.Select{Dir: app}, []string{"app", "app-edit"}},
		{"folder inside the source", api.Select{Dir: filepath.Join(app, "sub")}, []string{"app", "app-edit"}},
		{"folder holding the sources", api.Select{Dir: filepath.Dir(app)}, []string{"app", "app-edit", "other"}},
		{"unrelated folder", api.Select{Dir: f.state}, nil},
		{"inside a checkout", api.Select{Dir: filepath.Join(f.mount, "app-edit", "sub")}, []string{"app", "app-edit"}},
		{"the mount", api.Select{Dir: f.mount}, []string{"app", "app-edit", "other"}},
		{"idle", api.Select{Idle: time.Hour.Seconds()}, []string{"app", "app-edit"}},
		{"no diff", api.Select{NoDiff: true}, []string{"app", "other"}},
		{"all criteria", api.Select{Dir: app, Idle: time.Hour.Seconds(), NoDiff: true}, []string{"app"}},
	} {
		if got := f.picked(tt.sel); !slices.Equal(got, tt.want) {
			t.Errorf("%s: picked %v, want %v", tt.name, got, tt.want)
		}
	}
	if _, err := f.reg.Ls(context.Background(), api.LsArgs{Select: api.Select{Dir: filepath.Join(f.mount, "gone")}}); !isCode(err, "not_found") {
		t.Errorf("folder of a missing checkout in the mount: %v", err)
	}
}

// TestPrune checks that prune refuses to pick everything, that a dry run removes
// nothing, and that a checkout touched after it was picked stays.
func TestPrune(t *testing.T) {
	ctx := context.Background()
	f, app := selectFixture(t)
	if _, err := f.reg.Prune(ctx, api.PruneArgs{}); !isCode(err, "invalid") {
		t.Fatalf("prune without criteria: %v", err)
	}
	dry, err := f.reg.Prune(ctx, api.PruneArgs{Select: api.Select{NoDiff: true}, DryRun: true})
	if err != nil || len(dry.Removed) != 2 || len(f.ls().Checkouts) != 3 {
		t.Fatalf("dry run = %+v, %v; %d checkouts left", dry, err, len(f.ls().Checkouts))
	}
	picked := f.ls().Checkouts[0]
	s, err := f.reg.Enter(picked.Name)
	if err != nil {
		t.Fatal(err)
	}
	s.Touch()
	s.Done()
	if removed, err := f.reg.remove(ctx, picked.Name, picked.Touched); removed || err != nil {
		t.Fatalf("removing %s touched after it was picked = %v, %v", picked.Name, removed, err)
	}
	reply, err := f.reg.Prune(ctx, api.PruneArgs{Select: api.Select{Dir: app}})
	if err != nil || len(reply.Removed) != 2 {
		t.Fatalf("prune %s = %+v, %v", app, reply, err)
	}
	if got := f.picked(api.Select{}); !slices.Equal(got, []string{"other"}) {
		t.Fatalf("left %v, want other", got)
	}
}

// isCode reports whether err is a registry error with code.
func isCode(err error, code string) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Code == code
}
