package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/overlay"
)

// openLinks opens a daemon's registry and link manager under the node id,
// wired as serve wires them. A daemon that dials others must be opened after
// them, so that its links stop before theirs.
func openLinks(t *testing.T, node string) *links {
	t.Helper()
	root := t.TempDir()
	l := newLinks(nil)
	reg, err := checkout.Open(checkout.Config{
		State: filepath.Join(root, "state"), Mount: filepath.Join(root, "mount"), Node: node,
		Memory: overlay.NewMemory(64<<20, 1<<20, 0), BlobCache: 64 << 20,
		Fetch: l.fetch, Search: l.search, Retire: l.detach,
		Emit: func(e checkout.Event) {
			if l.emit != nil {
				l.emit(e)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	l.reg = reg
	t.Cleanup(func() {
		l.close()
		reg.Close()
	})
	return l
}

// pipeDial dials the named machines in-process: each session is piped into
// that machine's link manager, as `ssh HOST ovnfs link` pipes it into HOST's
// daemon. A machine's name goes to ended, unless nil, once a session there is over.
func pipeDial(hosts map[string]*links, ended chan<- string) dialer {
	return func(_ context.Context, host, _ string) (io.ReadWriteCloser, error) {
		h := hosts[host]
		if h == nil {
			return nil, fmt.Errorf("no machine %s", host)
		}
		here, there := net.Pipe()
		h.wg.Go(func() {
			h.inbound(there)
			if ended != nil {
				ended <- host
			}
		})
		return here, nil
	}
}

// linkFolder returns a new folder holding the files. Make it before opening
// the daemon that captures it, so that it outlives that daemon's registry.
func linkFolder(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// linkNames lists outbound links as "<checkout>@<host>".
func linkNames(found []*outLink) []string {
	names := make([]string, len(found))
	for i, o := range found {
		names[i] = o.checkout + "@" + o.to
	}
	return names
}

func TestPullSelectsAttachment(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        api.PullArgs
		attachments []string
		code        string
		message     string
	}{
		{name: "checkout required", code: "invalid", message: "needs a checkout"},
		{name: "no attachment", args: api.PullArgs{Checkout: "app"}, code: "not_found", message: "attach it first"},
		{name: "other checkout", args: api.PullArgs{Checkout: "app"}, attachments: []string{"application@a"}, code: "not_found"},
		{name: "unknown host", args: api.PullArgs{Checkout: "app", From: "missing"}, attachments: []string{"app@a"}, code: "not_found"},
		{name: "one disconnected attachment", args: api.PullArgs{Checkout: "app"}, attachments: []string{"app@a"}, code: "unavailable", message: "app@a is not connected"},
		{name: "ambiguous attachments", args: api.PullArgs{Checkout: "app"}, attachments: []string{"app@b", "app@a"}, code: "invalid", message: "a, b; choose --from HOST"},
		{name: "explicit disconnected host", args: api.PullArgs{Checkout: "app", From: "b"}, attachments: []string{"app@a", "app@b"}, code: "unavailable", message: "app@b is not connected"},
		{name: "ssh destination", args: api.PullArgs{Checkout: "app", From: "me@host"}, attachments: []string{"app@me@host", "app@other"}, code: "unavailable", message: "app@me@host is not connected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLinks(nil)
			defer l.cancel()
			for _, key := range tc.attachments {
				name, host, _ := strings.Cut(key, "@")
				l.out[key] = &outLink{checkout: name, to: host}
			}
			_, err := l.pull(context.Background(), tc.args)
			var he *checkout.Error
			if !errors.As(err, &he) || he.Code != tc.code {
				t.Fatalf("pull error = %v, want %s", err, tc.code)
			}
			if tc.message != "" && !strings.Contains(he.Message, tc.message) {
				t.Fatalf("pull error = %q, want %q", he.Message, tc.message)
			}
		})
	}
}

// TestPushReportsEachAttachment: push takes every attachment the filters
// match, in checkout and host order, and reports one that cannot push without
// failing the others.
func TestPushReportsEachAttachment(t *testing.T) {
	l := newLinks(nil)
	defer l.cancel()
	for _, key := range []string{"web@b", "app@me@b", "app@a"} {
		name, host, _ := strings.Cut(key, "@")
		l.out[key] = &outLink{checkout: name, to: host}
	}
	for _, tc := range []struct {
		args api.PushArgs
		want []string
	}{
		{args: api.PushArgs{}, want: []string{"app@a", "app@me@b", "web@b"}},
		{args: api.PushArgs{Checkout: "app"}, want: []string{"app@a", "app@me@b"}},
		{args: api.PushArgs{To: "b"}, want: []string{"web@b"}},
		{args: api.PushArgs{Checkout: "app", To: "me@b"}, want: []string{"app@me@b"}},
	} {
		r, err := l.push(context.Background(), tc.args)
		if err != nil {
			t.Fatalf("push %+v: %v", tc.args, err)
		}
		var got []string
		for _, p := range r.Pushed {
			got = append(got, p.Checkout+"@"+p.To)
			if !strings.Contains(p.Error, "not connected") {
				t.Errorf("push %s@%s error = %q, want not connected", p.Checkout, p.To, p.Error)
			}
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("push %+v reached %v, want %v", tc.args, got, tc.want)
		}
	}
	_, err := l.push(context.Background(), api.PushArgs{Checkout: "app", To: "c"})
	var he *checkout.Error
	if !errors.As(err, &he) || he.Code != "not_found" || !strings.Contains(he.Message, "checkout app is not attached to c") {
		t.Fatalf("push to no attachment = %v", err)
	}
}

// TestRemovingCheckoutEndsItsLinks: removing an attached checkout closes its
// session on every machine and stops its links' reconnect loops, and leaves
// other checkouts attached.
func TestRemovingCheckoutEndsItsLinks(t *testing.T) {
	dirs := map[string]string{"app": linkFolder(t, map[string]string{"f.txt": "app"}), "web": linkFolder(t, map[string]string{"f.txt": "web"})}
	a, b := openLinks(t, "n_a"), openLinks(t, "n_b")
	src := openLinks(t, "n_src")
	ended := make(chan string, 3)
	src.dial = pipeDial(map[string]*links{"a": a, "b": b}, ended)
	ctx := context.Background()
	for name, dir := range dirs {
		if _, err := src.reg.New(ctx, api.NewArgs{Dir: dir, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range [][2]string{{"app", "a"}, {"app", "b"}, {"web", "a"}} {
		if _, err := src.attach(ctx, api.AttachArgs{Checkout: at[0], To: at[1]}); err != nil {
			t.Fatal(err)
		}
	}

	if err := src.reg.Rm(ctx, api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	closed := map[string]bool{}
	for range 2 {
		select {
		case host := <-ended:
			closed[host] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("app's sessions closed on %v after app was removed, want a and b", closed)
		}
	}
	if !closed["a"] || !closed["b"] {
		t.Fatalf("sessions closed on %v after app was removed, want one on a and one on b", closed)
	}
	if got := linkNames(src.attached("", "")); !slices.Equal(got, []string{"web@a"}) {
		t.Fatalf("links after app was removed = %v, want web@a", got)
	}
	if pushed, err := src.push(ctx, api.PushArgs{}); err != nil || pushed.Pushed[0].Error != "" {
		t.Fatalf("push through web's link after app was removed: %+v, %v", pushed, err)
	}

	if err := src.reg.Rm(ctx, api.RmArgs{Checkout: "web"}); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		src.wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a link kept running after its checkout was removed")
	}
}

// TestFolderAttachedAgainKeepsItsLinks: attaching a folder to a second machine
// replaces the folder's checkout, and the link to the first machine has to
// survive that: it still serves that machine's reads and pulls.
func TestFolderAttachedAgainKeepsItsLinks(t *testing.T) {
	dir := linkFolder(t, map[string]string{"one.txt": "one"})
	a, b := openLinks(t, "n_a"), openLinks(t, "n_b")
	src := openLinks(t, "n_src")
	src.dial = pipeDial(map[string]*links{"a": a, "b": b}, nil)
	ctx := context.Background()
	first, err := src.attach(ctx, api.AttachArgs{Dir: dir, To: "a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := src.attach(ctx, api.AttachArgs{Dir: dir, To: "b"})
	if err != nil {
		t.Fatal(err)
	}
	name := second.Checkout
	if first.Checkout != name {
		t.Fatalf("the folder's checkout was %s, then %s", first.Checkout, name)
	}

	found := src.attached(name, "")
	if got := linkNames(found); !slices.Equal(got, []string{name + "@a", name + "@b"}) {
		t.Fatalf("links after attaching the folder to b = %v, want one to a and one to b", got)
	}
	key := checkout.LinkKey(found[0].offer.Node, found[0].offer.Source)
	wants := a.reg.Missing(key)
	if len(wants) == 0 {
		t.Fatal("a already holds the folder's content")
	}
	if err := a.fetch(ctx, key, wants); err != nil {
		t.Fatalf("a's read through its link after the folder was attached to b: %v", err)
	}
	if _, err := src.pull(ctx, api.PullArgs{Checkout: name, From: "a"}); err != nil {
		t.Fatalf("pull from a after the folder was attached to b: %v", err)
	}
}

// TestFolderAttachEndsLinksOfCheckoutsItDrops: a folder attach replaces every
// clean checkout of the folder with one; a replaced checkout whose name is
// gone afterwards has its links ended, as removing it would.
func TestFolderAttachEndsLinksOfCheckoutsItDrops(t *testing.T) {
	dir := linkFolder(t, map[string]string{"one.txt": "one"})
	a, b := openLinks(t, "n_a"), openLinks(t, "n_b")
	src := openLinks(t, "n_src")
	ended := make(chan string, 2)
	src.dial = pipeDial(map[string]*links{"a": a, "b": b}, ended)
	ctx := context.Background()
	if _, err := src.reg.New(ctx, api.NewArgs{Dir: dir, Name: "lane"}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.attach(ctx, api.AttachArgs{Checkout: "lane", To: "a"}); err != nil {
		t.Fatal(err)
	}
	reply, err := src.attach(ctx, api.AttachArgs{Dir: dir, To: "b"})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case host := <-ended:
		if host != "a" {
			t.Fatalf("the session on %s closed, want lane's on a", host)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lane's session stayed open after the folder attach replaced lane")
	}
	if got := linkNames(src.attached("", "")); !slices.Equal(got, []string{reply.Checkout + "@b"}) {
		t.Fatalf("links after the folder attach = %v, want only %s@b", got, reply.Checkout)
	}
}

// TestAttachFailsWhenItsCheckoutIsRemoved: an attach still waiting for the
// machine's answer fails once its checkout is removed, instead of waiting on
// a link that will never connect.
func TestAttachFailsWhenItsCheckoutIsRemoved(t *testing.T) {
	dir := linkFolder(t, map[string]string{"f.txt": "f"})
	src := openLinks(t, "n_src")
	dialed := make(chan struct{})
	src.dial = func(ctx context.Context, _, _ string) (io.ReadWriteCloser, error) {
		close(dialed)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx := context.Background()
	if _, err := src.reg.New(ctx, api.NewArgs{Dir: dir, Name: "app"}); err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() {
		_, err := src.attach(ctx, api.AttachArgs{Checkout: "app", To: "slow"})
		failed <- err
	}()
	select {
	case <-dialed:
	case err := <-failed:
		t.Fatalf("attach returned before dialing: %v", err)
	}

	if err := src.reg.Rm(ctx, api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		var he *checkout.Error
		if !errors.As(err, &he) || he.Code != "not_found" {
			t.Fatalf("attach of a checkout removed meanwhile = %v, want not_found", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach still waiting after its checkout was removed")
	}
}

// TestCheckoutRemovedWhileOfferedGetsNoLink: a checkout removed after attach
// took its snapshot, and before its link is registered, is never dialed for.
func TestCheckoutRemovedWhileOfferedGetsNoLink(t *testing.T) {
	dir := linkFolder(t, map[string]string{"f.txt": "f"})
	src := openLinks(t, "n_src")
	src.dial = func(context.Context, string, string) (io.ReadWriteCloser, error) {
		t.Error("dialed for a checkout that was removed")
		return nil, errors.New("unreachable")
	}
	ctx := context.Background()
	if _, err := src.reg.New(ctx, api.NewArgs{Dir: dir, Name: "app"}); err != nil {
		t.Fatal(err)
	}
	// The registry announces attach's snapshot from inside its offer.
	var once sync.Once
	src.emit = func(e checkout.Event) {
		if e.Name != "snapshotted" {
			return
		}
		once.Do(func() {
			if err := src.reg.Rm(ctx, api.RmArgs{Checkout: "app", StickySnapshots: true}); err != nil {
				t.Error(err)
			}
		})
	}
	_, err := src.attach(ctx, api.AttachArgs{Checkout: "app", To: "host"})
	var he *checkout.Error
	if !errors.As(err, &he) || he.Code != "not_found" {
		t.Fatalf("attach of a checkout removed while it was offered = %v, want not_found", err)
	}
}
