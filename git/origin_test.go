package git

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOrigin names HEAD while the upstream contains it and the merge base once
// it does not, and never passes on credentials embedded in the remote's URL.
func TestOrigin(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	if _, ok, err := r.Origin(ctx, testGit(t, r.Top, "rev-parse", "HEAD")); err != nil || ok {
		t.Fatalf("without an upstream: ok=%v err=%v", ok, err)
	}
	remote := t.TempDir()
	testGit(t, remote, "init", "-q", "--bare", "-b", "main")
	testGit(t, r.Top, "remote", "add", "origin", remote)
	testGit(t, r.Top, "push", "-q", "-u", "origin", "main")
	pushed := testGit(t, r.Top, "rev-parse", "HEAD")
	if o, ok, err := r.Origin(ctx, pushed); err != nil || !ok || o != (Origin{URL: remote, Commit: pushed}) {
		t.Fatalf("pushed HEAD: %+v %v %v", o, ok, err)
	}

	testWrite(t, filepath.Join(r.Top, "local.txt"), "not pushed")
	testGit(t, r.Top, "add", "local.txt")
	testGit(t, r.Top, "commit", "-qm", "local")
	head := testGit(t, r.Top, "rev-parse", "HEAD")
	if o, _, err := r.Origin(ctx, head); err != nil || o.Commit != pushed {
		t.Fatalf("unpushed HEAD: commit %s (%v), want the merge base %s", o.Commit, err, pushed)
	}

	for remoteURL, want := range map[string]string{
		"https://user:token@example.com/app.git": "https://example.com/app.git",
		"https://token@example.com/app.git":      "https://example.com/app.git",
		"ssh://git@example.com/app.git":          "ssh://git@example.com/app.git",
		"git@example.com:app.git":                "git@example.com:app.git",
	} {
		testGit(t, r.Top, "remote", "set-url", "origin", remoteURL)
		if o, _, err := r.Origin(ctx, head); err != nil || o.URL != want {
			t.Errorf("remote %s offered as %q (%v), want %q", remoteURL, o.URL, err, want)
		}
	}
}

// TestFetchOrigin fetches a commit that is no longer the branch tip, by id, into
// a bare mirror, and refuses origins git could misread.
func TestFetchOrigin(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	first := testGit(t, r.Top, "rev-parse", "HEAD")
	testWrite(t, filepath.Join(r.Top, "later.txt"), "later")
	testGit(t, r.Top, "add", "later.txt")
	testGit(t, r.Top, "commit", "-qm", "later")

	m, err := InitBare(filepath.Join(t.TempDir(), "mirror.git"))
	if err != nil {
		t.Fatal(err)
	}
	if have, err := m.HasOrigin(ctx, first); err != nil || have {
		t.Fatalf("empty mirror has %s: %v %v", first, have, err)
	}
	if err := m.FetchOrigin(ctx, Origin{URL: r.Top, Commit: first}); err != nil {
		t.Fatal(err)
	}
	if have, err := m.HasOrigin(ctx, first); err != nil || !have {
		t.Fatalf("after fetch: %v %v", have, err)
	}
	entries, err := m.LsTree(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Path == "later.txt" {
			t.Fatal("fetched a later commit's file")
		}
	}

	for _, bad := range []Origin{
		{URL: "--upload-pack=touch /tmp/x", Commit: first},
		{URL: r.Top, Commit: "HEAD"},
		{URL: r.Top, Commit: first[:12]},
	} {
		if err := m.FetchOrigin(ctx, bad); err == nil {
			t.Errorf("fetched %+v", bad)
		}
	}
}
