package remotefs

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
)

// changeFS adds the ownership and time changes osfs lacks, so a test share is a billy.Change.
type changeFS struct {
	billy.Filesystem
	root string
}

// Chmod changes a backing file's mode.
func (f changeFS) Chmod(p string, mode os.FileMode) error {
	return os.Chmod(filepath.Join(f.root, p), mode)
}

// Chown changes a backing file's owner, following symlinks.
func (f changeFS) Chown(p string, uid, gid int) error {
	return os.Chown(filepath.Join(f.root, p), uid, gid)
}

// Lchown changes a backing file's owner without following symlinks.
func (f changeFS) Lchown(p string, uid, gid int) error {
	return os.Lchown(filepath.Join(f.root, p), uid, gid)
}

// Chtimes changes a backing file's times.
func (f changeFS) Chtimes(p string, atime, mtime time.Time) error {
	return os.Chtimes(filepath.Join(f.root, p), atime, mtime)
}

// testShare serves the checkout "h" of a fresh directory tree.
func testShare(t *testing.T, writable bool) Share {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "h"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Share{FS: changeFS{osfs.New(root, osfs.WithBoundOS()), root}, Checkout: "h", Writable: writable}
}

// hostPath returns the backing path of a checkout path in a testShare.
func hostPath(share Share, p string) string {
	return filepath.Join(share.FS.(changeFS).root, share.Checkout, p)
}

// writeCheckout seeds a file directly in the checkout.
func writeCheckout(t *testing.T, share Share, p, data string) {
	t.Helper()
	full := hostPath(share, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startServer serves share on a fresh socket and returns a connected client.
func startServer(t *testing.T, share Share, opts Options) (*Client, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("remotefs share sockets are unsupported on Windows")
	}
	dir, err := os.MkdirTemp("/tmp", "rfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(l, share, opts) }()
	c, err := Dial(sock, "home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		l.Close()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return c, sock
}

// code returns the wire code of err, or "" for nil and non-protocol errors.
func code(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	if err != nil {
		return "transport: " + err.Error()
	}
	return ""
}

// TestServerAnswersTheVMMOpSet walks a writable share through every op the VMM sends.
func TestServerAnswersTheVMMOpSet(t *testing.T) {
	share := testShare(t, true)
	writeCheckout(t, share, "src/main.go", "package main\n")
	c, _ := startServer(t, share, Options{})

	root, err := c.Stat("")
	if err != nil || root.Kind != KindDir {
		t.Fatalf("stat root = %+v, %v", root, err)
	}

	dir, err := c.Mkdir("docs", 0o750, 0, 0)
	if err != nil || dir.Kind != KindDir || *dir.Mode != 0o750 {
		t.Fatalf("mkdir = %+v, %v", dir, err)
	}
	f, err := c.Create("docs/a.txt", 0o664, 0, 0, true, false)
	if err != nil || f.Kind != KindFile || f.Size != 0 || *f.Mode != 0o664 {
		t.Fatalf("create = %+v, %v", f, err)
	}
	if n, err := c.Write("docs/a.txt", 0, []byte("hello world")); err != nil || n != 11 {
		t.Fatalf("write = %d, %v", n, err)
	}
	if n, err := c.Write("docs/a.txt", 11, []byte("!")); err != nil || n != 1 {
		t.Fatalf("append write = %d, %v", n, err)
	}
	if err := c.Sync("docs/a.txt", false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := c.Sync("docs", true); err != nil {
		t.Fatalf("sync dir: %v", err)
	}
	if got, err := c.Read("docs/a.txt", 6, 100); err != nil || string(got) != "world!" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if got, err := c.Read("docs/a.txt", 50, 10); err != nil || len(got) != 0 {
		t.Fatalf("read past EOF = %q, %v", got, err)
	}
	if _, err := c.Create("docs/a.txt", 0o644, 0, 0, false, false); err != nil {
		t.Fatalf("open existing: %v", err)
	}
	if got, _ := c.Read("docs/a.txt", 0, 5); string(got) != "hello" {
		t.Fatalf("create without trunc changed data: %q", got)
	}

	size, mode, mtime := uint64(5), uint32(0o600), uint64(1_000_000)
	st, err := c.Setattr("docs/a.txt", SetAttr{Size: &size, Mode: &mode, Mtime: &mtime})
	if err != nil || st.Size != 5 || *st.Mode != 0o600 || st.Mtime != mtime {
		t.Fatalf("setattr = %+v, %v", st, err)
	}
	if st, err := c.Create("docs/a.txt", 0o644, 0, 0, false, true); err != nil || st.Size != 0 || *st.Mode != 0o600 {
		t.Fatalf("create trunc = %+v, %v", st, err)
	}

	if err := os.Symlink("../src", hostPath(share, "docs/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", hostPath(share, "docs/dangling")); err != nil {
		t.Fatal(err)
	}
	entries, err := c.List("docs")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	kinds := map[string]Kind{}
	for _, e := range entries {
		names = append(names, e.Name)
		kinds[e.Name] = e.Kind
	}
	if want := []string{"a.txt", "dangling", "link"}; !slices.Equal(names, want) {
		t.Fatalf("list names = %v, want %v", names, want)
	}
	if kinds["link"] != KindDir || kinds["dangling"] != KindFile || kinds["a.txt"] != KindFile {
		t.Fatalf("list kinds = %v", kinds)
	}
	if st, err := c.Stat("docs/link/main.go"); err != nil || st.Size != 13 {
		t.Fatalf("stat through symlink = %+v, %v", st, err)
	}

	if err := c.Rename("docs/a.txt", "src/main.go", true); code(err) != CodeExists {
		t.Fatalf("rename noreplace onto existing = %v", err)
	}
	if err := c.Rename("docs/a.txt", "docs/b.txt", true); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := c.Rename("docs/b.txt", "src/main.go", false); err != nil {
		t.Fatalf("rename replace: %v", err)
	}
	if _, err := c.Stat("docs/b.txt"); code(err) != CodeNotFound || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat renamed-away = %v", err)
	}
	// osfs resolves the final symlink on Remove, so symlink unlinks are covered by the export test.
	for _, link := range []string{"docs/dangling", "docs/link"} {
		if err := os.Remove(hostPath(share, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Rmdir("docs"); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	if err := c.Unlink("src/main.go"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
}

// TestSymlinksStayInsideTheCheckout checks the server resolves symlinks itself and never leaves the checkout.
func TestSymlinksStayInsideTheCheckout(t *testing.T) {
	share := testShare(t, true)
	writeCheckout(t, share, "real/f", "inside")
	outside := filepath.Join(filepath.Dir(hostPath(share, "")), "secret")
	if err := os.WriteFile(outside, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"rel":   "real",
		"up":    "../../real",
		"abs":   outside,
		"loop":  "loop",
		"chain": "rel/../rel",
	} {
		if err := os.Symlink(target, hostPath(share, link)); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := startServer(t, share, Options{})
	tests := []struct {
		path string
		want string
		kind Kind
	}{
		{"rel/f", "", KindFile},
		{"chain/f", "", KindFile},
		{"up/f", "", KindFile}, // ".." clamps at the checkout root like a chroot
		{"rel", "", KindDir},
		{"abs", "", KindFile}, // an escaping link is described as itself
		{"loop", "", KindFile},
		{"loop/f", CodeIO, ""},
	}
	for _, tt := range tests {
		st, err := c.Stat(tt.path)
		if code(err) != tt.want || st.Kind != tt.kind {
			t.Errorf("stat %s = %v, %q; want kind %q code %q", tt.path, st.Kind, code(err), tt.kind, tt.want)
		}
	}
	if got, err := c.Read("abs", 0, 10); code(err) != CodeNotFound {
		t.Fatalf("read through an absolute symlink = %q, %v", got, err)
	}
	if got, err := c.Read("up/f", 0, 10); err != nil || string(got) != "inside" {
		t.Fatalf("read through a clamped symlink = %q, %v", got, err)
	}
	if _, err := c.Create("rel/new", 0o644, 0, 0, true, false); err != nil {
		t.Fatalf("create through a symlinked parent: %v", err)
	}
	if _, err := os.Stat(hostPath(share, "real/new")); err != nil {
		t.Fatalf("created file not in the link target: %v", err)
	}
}

// TestServerErrorCodes checks the codes the VMM maps to errnos.
func TestServerErrorCodes(t *testing.T) {
	share := testShare(t, true)
	writeCheckout(t, share, "dir/file", "x")
	c, _ := startServer(t, share, Options{})
	tests := []struct {
		name string
		call func() error
		want string
	}{
		{"stat missing", func() error { _, err := c.Stat("nope"); return err }, CodeNotFound},
		{"dotdot path", func() error { _, err := c.Stat("../h"); return err }, CodeBadRequest},
		{"absolute path", func() error { _, err := c.Stat("/dir"); return err }, CodeBadRequest},
		{"empty segment", func() error { _, err := c.Stat("dir//file"); return err }, CodeBadRequest},
		{"list file", func() error { _, err := c.List("dir/file"); return err }, CodeNotDir},
		{"read dir", func() error { _, err := c.Read("dir", 0, 1); return err }, CodeIsDir},
		{"read too big", func() error { _, err := c.Read("dir/file", 0, MaxRead+1); return err }, CodeTooBig},
		{"create excl exists", func() error { _, err := c.Create("dir/file", 0o644, 0, 0, true, false); return err }, CodeExists},
		{"create root", func() error { _, err := c.Create("", 0o644, 0, 0, true, false); return err }, CodeBadRequest},
		{"create dir", func() error { _, err := c.Create("dir", 0o644, 0, 0, false, false); return err }, CodeIsDir},
		{"create missing parent", func() error { _, err := c.Create("nope/f", 0o644, 0, 0, true, false); return err }, CodeNotFound},
		{"mkdir exists", func() error { _, err := c.Mkdir("dir", 0o755, 0, 0); return err }, CodeExists},
		{"mkdir missing parent", func() error { _, err := c.Mkdir("nope/d", 0o755, 0, 0); return err }, CodeNotFound},
		{"mkdir under file", func() error { _, err := c.Mkdir("dir/file/d", 0o755, 0, 0); return err }, CodeNotDir},
		{"write missing", func() error { _, err := c.Write("nope", 0, []byte("x")); return err }, CodeNotFound},
		{"unlink dir", func() error { return c.Unlink("dir") }, CodeIsDir},
		{"rmdir file", func() error { return c.Rmdir("dir/file") }, CodeNotDir},
		{"rmdir non-empty", func() error { return c.Rmdir("dir") }, CodeNotEmpty},
		{"rename missing", func() error { return c.Rename("nope", "x", false) }, CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := code(tt.call()); got != tt.want {
				t.Fatalf("code = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestReadOnlyShareRefusesMutations checks a read-only share serves reads and answers mutations rofs.
func TestReadOnlyShareRefusesMutations(t *testing.T) {
	share := testShare(t, false)
	writeCheckout(t, share, "f", "data")
	c, _ := startServer(t, share, Options{})
	if got, err := c.Read("f", 0, 4); err != nil || string(got) != "data" {
		t.Fatalf("read = %q, %v", got, err)
	}
	size := uint64(0)
	for name, call := range map[string]func() error{
		"create":  func() error { _, err := c.Create("g", 0o644, 0, 0, true, false); return err },
		"mkdir":   func() error { _, err := c.Mkdir("d", 0o755, 0, 0); return err },
		"write":   func() error { _, err := c.Write("f", 0, []byte("x")); return err },
		"setattr": func() error { _, err := c.Setattr("f", SetAttr{Size: &size}); return err },
		"unlink":  func() error { return c.Unlink("f") },
		"rmdir":   func() error { return c.Rmdir("f") },
		"rename":  func() error { return c.Rename("f", "g", false) },
	} {
		if got := code(call()); got != CodeRofs {
			t.Errorf("%s code = %q, want rofs", name, got)
		}
	}
	if got, _ := os.ReadFile(hostPath(share, "f")); string(got) != "data" {
		t.Fatalf("read-only share changed: %q", got)
	}
}

// TestIDMapsTranslateOwnership checks replies show guest ids and creations adopt mapped owners.
func TestIDMapsTranslateOwnership(t *testing.T) {
	share := testShare(t, true)
	writeCheckout(t, share, "f", "x")
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	opts := Options{UIDs: []IDRange{{Guest: 1000, Host: uid, Count: 1}}, GIDs: []IDRange{{Guest: 1001, Host: gid, Count: 1}}}
	c, _ := startServer(t, share, opts)
	st, err := c.Stat("f")
	if err != nil || *st.UID != 1000 || *st.GID != 1001 {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	for _, caller := range [][2]uint32{{1000, 1001}, {4242, 4243}} {
		st, err := c.Create(fmt.Sprintf("new%d", caller[0]), 0o644, caller[0], caller[1], true, false)
		if err != nil || *st.UID != 1000 || *st.GID != 1001 {
			t.Fatalf("create as %v = %+v, %v", caller, st, err)
		}
	}
	guestUID := uint32(1000)
	if st, err := c.Setattr("f", SetAttr{UID: &guestUID}); err != nil || *st.UID != 1000 {
		t.Fatalf("chown to mapped uid = %+v, %v", st, err)
	}
}

// TestPeerOutsideAllowListIsDropped checks the peer-uid gate closes foreign connections.
func TestPeerOutsideAllowListIsDropped(t *testing.T) {
	share := testShare(t, true)
	c, _ := startServer(t, share, Options{PeerUIDs: []uint32{uint32(os.Geteuid()) + 1}})
	if _, err := c.Stat(""); err == nil || code(err) == "" || !strings.HasPrefix(code(err), "transport") {
		t.Fatalf("stat from foreign peer = %v, want a dropped connection", err)
	}
}

// TestFramingViolationsCloseTheConnection checks oversized and unknown frames end the
// connection while malformed requests are answered.
func TestFramingViolationsCloseTheConnection(t *testing.T) {
	share := testShare(t, true)
	tests := []struct {
		name   string
		frame  []byte
		answer byte
	}{
		{"bad json", frameBytes(FrameReq, 3, `{"op":`), FrameErr},
		{"unknown op", frameBytes(FrameReq, 3, `{"op":"chmod","path":"a"}`), FrameErr},
		{"missing field", frameBytes(FrameReq, 3, `{"op":"read","tag":"t","path":"a"}`), FrameErr},
		{"short data header", frameBytes(FrameReqData, 3, "\x01"), FrameErr},
		{"unknown type", frameBytes(9, 3, `{}`), 0},
		{"oversized", append(frameBytes(FrameReq, 3, ""), 0)[:HeaderLen], 0},
	}
	tests[len(tests)-1].frame[3] = 0xff // length 0xff000000 > MaxFrame
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, srv := net.Pipe()
			defer client.Close()
			s := &server{share: share}
			go func() {
				s.serveConn(srv)
				srv.Close()
			}()
			go client.Write(tt.frame)
			header := make([]byte, HeaderLen)
			_, err := io.ReadFull(client, header)
			if tt.answer == 0 {
				if err == nil {
					t.Fatalf("connection answered %v, want close", header)
				}
				return
			}
			if err != nil || header[4] != tt.answer || header[5] != 3 {
				t.Fatalf("header = %v, %v", header, err)
			}
		})
	}
}

// TestListenReplacesOnlyStaleSockets checks socket creation, mode and stale-socket handling.
func TestListenReplacesOnlyStaleSockets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remotefs share sockets are unsupported on Windows")
	}
	dir, err := os.MkdirTemp("/tmp", "rfs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(sock); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", info.Mode(), err)
	}
	if _, err := Listen(sock); err == nil {
		t.Fatal("Listen replaced a live socket")
	}
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	l.Close()
	l, err = Listen(sock)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	l.Close()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(plain); err == nil {
		t.Fatal("Listen replaced a regular file")
	}
}
