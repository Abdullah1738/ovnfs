package smb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/dirbottom"
	"github.com/stencil-hq/ovnfs/ntinfo"
	"github.com/stencil-hq/ovnfs/overlay"
)

// testBackend supplies stable file identities and volume statistics without importing the executable.
type testBackend struct {
	*overlay.FS
	mu   sync.Mutex
	ids  map[string]uint64
	next uint64
}

// FileID assigns an identity that is unaffected by an overlay copy-up.
func (b *testBackend) FileID(name string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	name = overlay.Clean(name)
	if id := b.ids[name]; id != 0 {
		return id
	}
	b.next++
	b.ids[name] = b.next
	return b.next
}

// Rename moves identity mappings for the renamed object and all its descendants.
func (b *testBackend) Rename(from, to string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.FS.Rename(from, to); err != nil {
		return err
	}
	from, to = overlay.Clean(from), overlay.Clean(to)
	moved := make(map[string]uint64)
	for p, id := range b.ids {
		if p == from || strings.HasPrefix(p, from+"/") {
			moved[to+strings.TrimPrefix(p, from)] = id
			delete(b.ids, p)
		}
	}
	for p := range b.ids {
		if p == to || strings.HasPrefix(p, to+"/") {
			delete(b.ids, p)
		}
	}
	maps.Copy(b.ids, moved)
	return nil
}

// Remove retires identities only after the filesystem deletion succeeds.
func (b *testBackend) Remove(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.FS.Remove(name); err != nil {
		return err
	}
	name = overlay.Clean(name)
	for p := range b.ids {
		if p == name || strings.HasPrefix(p, name+"/") {
			delete(b.ids, p)
		}
	}
	return nil
}

// Cacheable allows caching because the fixture controls every backend mutation.
func (b *testBackend) Cacheable(string) bool { return true }

// Statfs returns deterministic volume statistics on every test platform.
func (b *testBackend) Statfs() (Statfs, error) {
	return Statfs{BlockSize: 4096, Blocks: 1 << 20, Free: 1 << 19, Avail: 1 << 19, Files: 1 << 20, FilesFree: 1 << 19}, nil
}

// smbFixture owns an overlay and its listening server for a single test.
type smbFixture struct {
	address      string
	lower, upper string
	fs           *overlay.FS
	server       *Server
}

// flush moves memory files into the upper so assertions can read it from disk.
func (f smbFixture) flush(t *testing.T) {
	t.Helper()
	if err := f.fs.Flush(); err != nil {
		t.Fatal(err)
	}
}

// startSMB prepares on-disk inputs before starting Serve on an ephemeral loopback port.
func startSMB(t *testing.T, users map[string]string, seed func(lower, upper string)) smbFixture {
	t.Helper()
	return startSMBWith(t, func(s *Server) { s.Users = users }, seed)
}

// startSMBWith is startSMB with the server configured before it serves.
func startSMBWith(t *testing.T, configure func(*Server), seed func(lower, upper string)) smbFixture {
	t.Helper()
	lower, upper := t.TempDir(), t.TempDir()
	if seed != nil {
		seed(lower, upper)
	}
	fs, err := overlay.New(upper, nil, dirbottom.New(lower), overlay.NewMemory(64<<20, 1<<20, 0))
	if err != nil {
		t.Fatal(err)
	}
	backend := &testBackend{FS: fs, ids: make(map[string]uint64)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{FS: backend, Share: "test"}
	configure(server)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("Serve did not stop after listener close")
		}
	})
	return smbFixture{address: listener.Addr().String(), lower: lower, upper: upper, fs: fs, server: server}
}

// putDiskFile seeds a lower or upper file, including any required parent directories.
func putDiskFile(t *testing.T, root, name, data string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// diskEquals checks the physical layer, independently of the SMB read path.
func diskEquals(t *testing.T, root, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("disk %s: %q, want %q", name, got, want)
	}
}

// guestClient establishes an unsigned guest session and mounts the data share.
func guestClient(t *testing.T, fixture smbFixture) *wireClient {
	t.Helper()
	c := newWireClient(t, fixture.address)
	c.negotiate([]uint16{0x0202, 0x0210, 0x0302, 0x0311})
	c.expectStatus(c.login("", "", true), wireSuccess)
	c.expectStatus(c.treeConnect("test"), wireSuccess)
	return c
}

// mustOpen turns an unexpected CREATE status into a failure at the test call site.
func mustOpen(t *testing.T, c *wireClient, name string, disposition, options uint32) wireID {
	t.Helper()
	id, r := c.create(name, disposition, options)
	c.expectStatus(r, wireSuccess)
	return id
}

// readEquals compares the bytes exposed through SMB, not the backing filesystem.
func readEquals(t *testing.T, c *wireClient, id wireID, want string) {
	t.Helper()
	got, r := c.read(id, 0, uint32(len(want)+64))
	c.expectStatus(r, wireSuccess)
	if string(got) != want {
		t.Fatalf("SMB read %q, want %q", got, want)
	}
}

// internalID reads FILE_INTERNAL_INFORMATION and validates its public wire shape.
func internalID(t *testing.T, c *wireClient, id wireID) uint64 {
	t.Helper()
	data, r := c.queryInfo(id, 6)
	c.expectStatus(r, wireSuccess)
	if len(data) != 8 {
		t.Fatalf("FILE_INTERNAL_INFORMATION length %d", len(data))
	}
	value := binary.LittleEndian.Uint64(data)
	if value == 0 {
		t.Fatal("server returned zero file identity")
	}
	return value
}

// TestSMBGuestShares covers both NTLM token encodings and disk, IPC, and missing shares.
func TestSMBGuestShares(t *testing.T) {
	fixture := startSMB(t, nil, nil)
	for _, spnego := range []bool{false, true} {
		t.Run(fmt.Sprintf("SPNEGO=%t", spnego), func(t *testing.T) {
			c := newWireClient(t, fixture.address)
			c.negotiate([]uint16{0x0210, 0x0302, 0x0311})
			c.expectStatus(c.login("", "", spnego), wireSuccess)
			r := c.treeConnect("test")
			c.expectStatus(r, wireSuccess)
			if len(r.body) < 16 || r.body[2] != 1 {
				t.Fatal("data share is not a disk share")
			}
			r = c.treeConnect("IPC$")
			c.expectStatus(r, wireSuccess)
			if len(r.body) < 16 || r.body[2] != 2 {
				t.Fatal("IPC$ is not a pipe share")
			}
			c.expectStatus(c.treeConnect("does-not-exist"), wireBadShare)
		})
	}
}

// TestSMBWriteRead checks creation, physical upper writes, and the EOF status contract.
func TestSMBWriteRead(t *testing.T) {
	fixture := startSMB(t, nil, nil)
	c := guestClient(t, fixture)
	id := mustOpen(t, c, "roundtrip", 2, 0x40)
	data := "overlay SMB round trip\x00with binary data"
	c.expectStatus(c.write(id, 0, []byte(data)), wireSuccess)
	readEquals(t, c, id, data)
	_, r := c.read(id, uint64(len(data)), 1)
	c.expectStatus(r, wireEOF)
	_, r = c.read(id, uint64(len(data)+100), 8)
	c.expectStatus(r, wireEOF)
	c.expectStatus(c.close(id), wireSuccess)
	fixture.flush(t)
	diskEquals(t, fixture.upper, "roundtrip", data)
	if _, err := os.Stat(filepath.Join(fixture.lower, "roundtrip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new file unexpectedly touched lower: %v", err)
	}
}

// TestSMBCopyUp verifies that opening a lower file for mutation leaves lower bytes unchanged.
func TestSMBCopyUp(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "lower-file", "original content") })
	c := guestClient(t, fixture)
	id := mustOpen(t, c, "lower-file", 1, 0x40)
	fixture.flush(t)
	diskEquals(t, fixture.upper, "lower-file", "original content")
	diskEquals(t, fixture.lower, "lower-file", "original content")
	before := internalID(t, c, id)
	c.expectStatus(c.write(id, 0, []byte("modified")), wireSuccess)
	readEquals(t, c, id, "modified content")
	if got := internalID(t, c, id); got != before {
		t.Fatalf("copy-up changed file identity from %d to %d", before, got)
	}
	c.expectStatus(c.close(id), wireSuccess)
	diskEquals(t, fixture.lower, "lower-file", "original content")
	fixture.flush(t)
	diskEquals(t, fixture.upper, "lower-file", "modified content")
}

// TestSMBDirectoryPagination requires every entry exactly once across many small response pages.
func TestSMBDirectoryPagination(t *testing.T) {
	const count = 2000
	fixture := startSMB(t, nil, func(lower, upper string) {
		for i := range count {
			putDiskFile(t, lower, fmt.Sprintf("many/file-%04d", i), "")
		}
	})
	c := guestClient(t, fixture)
	id := mustOpen(t, c, "many", 1, 1)
	seen := make(map[string]bool, count)
	pages := 0
	ended := false
	for page := range count + 10 {
		names, r := c.queryDirectory(id, page == 0, 512)
		if r.status == wireNoMoreFiles {
			ended = true
			break
		}
		if r.status != wireSuccess && r.status != 0x80000005 {
			c.expectStatus(r, wireSuccess)
		}
		if len(names) == 0 {
			t.Fatal("successful empty directory page never advances cursor")
		}
		pages++
		for _, name := range names {
			if name == "." || name == ".." {
				continue
			}
			if seen[name] {
				t.Fatalf("duplicate directory entry %q", name)
			}
			seen[name] = true
		}
	}
	if !ended {
		t.Fatal("enumeration did not end with STATUS_NO_MORE_FILES")
	}
	if pages < 2 {
		t.Fatal("directory response did not paginate")
	}
	if len(seen) != count {
		t.Fatalf("got %d entries, want %d", len(seen), count)
	}
	for i := range count {
		if name := fmt.Sprintf("file-%04d", i); !seen[name] {
			t.Errorf("missing %s", name)
		}
	}
	_, r := c.queryDirectory(id, false, 512)
	c.expectStatus(r, wireNoMoreFiles)
	c.expectStatus(c.close(id), wireSuccess)
}

// TestSMBDirectoryPaginationRefreshesMetadata keeps enumeration cookies stable
// while reporting current archive attributes and omitting entries removed later.
func TestSMBDirectoryPaginationRefreshesMetadata(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) {
		putDiskFile(t, lower, "a-vanished.rlib", "remove before archive")
		putDiskFile(t, lower, "b-archive.rlib", "old")
		putDiskFile(t, lower, "c-vanished.rlib", "remove after archive")
	})
	c := guestClient(t, fixture)
	archive := mustOpen(t, c, "b-archive.rlib", 1, 0x40)
	dir := mustOpen(t, c, "", 1, 1)
	type entry struct {
		name          string
		cookie        uint32
		size, written uint64
	}
	page := func(restart bool) (entry, wireReply) {
		t.Helper()
		body := make([]byte, 32)
		binary.LittleEndian.PutUint16(body, 33)
		body[2], body[3] = 1, 2
		copy(body[8:], dir[:])
		binary.LittleEndian.PutUint32(body[28:], 4096)
		if restart {
			body[3] |= 1
			pattern := wireUTF16("*")
			binary.LittleEndian.PutUint16(body[24:], 96)
			binary.LittleEndian.PutUint16(body[26:], uint16(len(pattern)))
			body = append(body, pattern...)
		}
		r := c.exchange(14, body)
		if r.status != wireSuccess {
			return entry{}, r
		}
		if len(r.body) < 8 {
			t.Fatal("short QUERY_DIRECTORY response")
		}
		data := c.wireSecurity(r, int(binary.LittleEndian.Uint16(r.body[2:])), int(binary.LittleEndian.Uint32(r.body[4:])))
		if len(data) < 64 || binary.LittleEndian.Uint32(data) != 0 {
			t.Fatal("expected one complete directory entry")
		}
		n := int(binary.LittleEndian.Uint32(data[60:]))
		if n > len(data)-64 || n%2 != 0 {
			t.Fatal("invalid directory entry name")
		}
		return entry{
			name:    wireString(data[64 : 64+n]),
			cookie:  binary.LittleEndian.Uint32(data[4:]),
			size:    binary.LittleEndian.Uint64(data[40:]),
			written: binary.LittleEndian.Uint64(data[24:]),
		}, r
	}
	first, r := page(true)
	c.expectStatus(r, wireSuccess)
	if first.name != "." || first.cookie != 1 {
		t.Fatalf("first snapshot entry = %+v, want dot with cookie 1", first)
	}

	// The directory snapshot now contains the archive's old, three-byte EOF.
	data := bytes.Repeat([]byte("archive record\n"), 4096)
	c.expectStatus(c.write(archive, 0, data), wireSuccess)
	written := uint64(time.Date(2037, 1, 2, 3, 4, 5, 0, time.UTC).Unix()+11644473600) * 10000000
	basic := make([]byte, 40)
	binary.LittleEndian.PutUint64(basic[16:], written)
	c.expectStatus(c.setInfo(archive, 4, basic), wireSuccess)
	for _, name := range []string{"a-vanished.rlib", "c-vanished.rlib"} {
		id := mustOpen(t, c, name, 1, 0x40)
		c.expectStatus(c.disposition(id, true), wireSuccess)
		c.expectStatus(c.close(id), wireSuccess)
	}
	added := mustOpen(t, c, "d-added.rlib", 2, 0x40)
	c.expectStatus(c.close(added), wireSuccess)

	seen := map[string]bool{first.name: true}
	previous := first.cookie
	ended := false
	for range 8 {
		e, r := page(false)
		if r.status == wireNoMoreFiles {
			ended = true
			break
		}
		c.expectStatus(r, wireSuccess)
		if seen[e.name] || e.cookie <= previous {
			t.Fatalf("repeated entry or nonadvancing cookie: %+v after %d", e, previous)
		}
		seen[e.name] = true
		previous = e.cookie
		switch e.name {
		case "..":
		case "b-archive.rlib":
			if e.cookie != 4 {
				t.Fatalf("removal changed the archive's snapshot cookie: %d, want 4", e.cookie)
			}
			if e.size != uint64(len(data)) || e.written != written {
				t.Fatalf("archive metadata is stale: EOF=%d mtime=%d, want EOF=%d mtime=%d", e.size, e.written, len(data), written)
			}
		default:
			t.Fatalf("snapshot returned removed or newly added entry %q", e.name)
		}
	}
	if !ended || !seen["b-archive.rlib"] || !seen[".."] || len(seen) != 3 {
		t.Fatalf("incomplete enumeration: ended=%v entries=%v", ended, seen)
	}
	got, r := c.read(archive, 0, uint32(len(data)))
	c.expectStatus(r, wireSuccess)
	if !bytes.Equal(got, data) {
		t.Fatal("archive data changed while enumerating")
	}
	c.expectStatus(c.close(dir), wireSuccess)
	c.expectStatus(c.close(archive), wireSuccess)
}

// TestSMBRenameReplacement checks collision policy, replaced content, and persistent identity.
func TestSMBRenameReplacement(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) {
		putDiskFile(t, lower, "source", "source data")
		putDiskFile(t, lower, "destination", "destination data")
	})
	c := guestClient(t, fixture)
	id := mustOpen(t, c, "source", 1, 0x40)
	before := internalID(t, c, id)
	c.expectStatus(c.rename(id, "destination", false), wireCollision)
	other := mustOpen(t, c, "destination", 1, 0x40)
	readEquals(t, c, other, "destination data")
	c.expectStatus(c.close(other), wireSuccess)
	c.expectStatus(c.rename(id, "destination", true), wireSuccess)
	if after := internalID(t, c, id); after != before {
		t.Fatalf("rename changed identity: %d -> %d", before, after)
	}
	readEquals(t, c, id, "source data")
	c.expectStatus(c.close(id), wireSuccess)
	reopened := mustOpen(t, c, "destination", 1, 0x40)
	if after := internalID(t, c, reopened); after != before {
		t.Fatalf("reopened renamed file changed identity: %d -> %d", before, after)
	}
	readEquals(t, c, reopened, "source data")
	c.expectStatus(c.close(reopened), wireSuccess)
	_, r := c.create("source", 1, 0x40)
	c.expectStatus(r, 0xc0000034)
	diskEquals(t, fixture.lower, "source", "source data")
	diskEquals(t, fixture.lower, "destination", "destination data")
	fixture.flush(t)
	diskEquals(t, fixture.upper, "destination", "source data")
}

// TestSMBDeleteOnClose checks create-option deletion and disposition-based lower whiteouts.
func TestSMBDeleteOnClose(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "lower-delete", "keep lower") })
	c := guestClient(t, fixture)
	id := mustOpen(t, c, "new-delete", 2, 0x1040)
	c.expectStatus(c.write(id, 0, []byte("temporary")), wireSuccess)
	c.expectStatus(c.close(id), wireSuccess)
	_, r := c.create("new-delete", 1, 0x40)
	c.expectStatus(r, 0xc0000034)
	fixture.flush(t)
	if _, err := os.Lstat(filepath.Join(fixture.upper, "new-delete")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("upper file survived delete-on-close: %v", err)
	}
	id = mustOpen(t, c, "lower-delete", 1, 0x40)
	c.expectStatus(c.disposition(id, true), wireSuccess)
	c.expectStatus(c.close(id), wireSuccess)
	_, r = c.create("lower-delete", 1, 0x40)
	c.expectStatus(r, 0xc0000034)
	if _, err := os.Stat(filepath.Join(fixture.upper, ".wh.lower-delete")); err != nil {
		t.Fatalf("lower deletion left no whiteout: %v", err)
	}
	diskEquals(t, fixture.lower, "lower-delete", "keep lower")
}

// TestSMBNonemptyDirectoryDisposition requires rejection before any child can be removed.
func TestSMBNonemptyDirectoryDisposition(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "directory/child", "child data") })
	c := guestClient(t, fixture)
	id := mustOpen(t, c, "directory", 1, 1)
	c.expectStatus(c.disposition(id, true), wireNotEmpty)
	c.expectStatus(c.close(id), wireSuccess)
	child := mustOpen(t, c, `directory\child`, 1, 0x40)
	readEquals(t, c, child, "child data")
	c.expectStatus(c.close(child), wireSuccess)
}

// TestSMBRelatedCompound checks all-ones related handles and inherited session/tree identifiers.
func TestSMBRelatedCompound(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "compound", "compound data") })
	c := guestClient(t, fixture)
	replies := c.compound("compound")
	for _, r := range replies {
		c.expectStatus(r, wireSuccess)
	}
	query := replies[1]
	if len(query.body) < 8 {
		t.Fatal("short compound QUERY_INFO response")
	}
	data := c.wireSecurity(query, int(binary.LittleEndian.Uint16(query.body[2:])), int(binary.LittleEndian.Uint32(query.body[4:])))
	if len(data) != 8 {
		t.Fatalf("compound internal information has length %d", len(data))
	}
	id := mustOpen(t, c, "compound", 1, 0x40)
	if got := internalID(t, c, id); got != binary.LittleEndian.Uint64(data) {
		t.Fatalf("compound queried wrong related file: identity %d", got)
	}
	readEquals(t, c, id, "compound data")
	c.expectStatus(c.close(id), wireSuccess)
}

// TestSMBByteRangeLock checks immediate conflict, unlock, and successful subsequent acquisition.
func TestSMBByteRangeLock(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "locked", "0123456789") })
	c := guestClient(t, fixture)
	first := mustOpen(t, c, "locked", 1, 0x40)
	second := mustOpen(t, c, "locked", 1, 0x40)
	c.expectStatus(c.lock(first, 0, 8, 2|16), wireSuccess)
	c.expectStatus(c.lock(second, 4, 4, 2|16), wireLockDenied)
	c.expectStatus(c.lock(first, 0, 8, 4), wireSuccess)
	c.expectStatus(c.lock(second, 4, 4, 2|16), wireSuccess)
	c.expectStatus(c.lock(second, 4, 4, 4), wireSuccess)
	c.expectStatus(c.close(first), wireSuccess)
	c.expectStatus(c.close(second), wireSuccess)
}

// TestSMBPOSIXMetadata checks Linux wire types, final symlinks, empty EAs, and attribute-only reparse creation.
func TestSMBPOSIXMetadata(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) {
		putDiskFile(t, lower, "file", "data")
		putDiskFile(t, lower, "dir/script", "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(lower, "dir/script"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("file", filepath.Join(lower, "link")); err != nil {
			t.Fatal(err)
		}
	})
	c := guestClient(t, fixture)
	create := func(name string, disposition, options, access uint32) wireID {
		body := wireCreateBody(name, disposition, options)
		binary.LittleEndian.PutUint32(body[24:], access)
		offset := (64 + len(body) + 7) &^ 7
		body = append(body, make([]byte, offset-64-len(body))...)
		context := make([]byte, 40)
		binary.LittleEndian.PutUint16(context[4:], 16)
		binary.LittleEndian.PutUint16(context[6:], 16)
		binary.LittleEndian.PutUint16(context[10:], 32)
		binary.LittleEndian.PutUint32(context[12:], 4)
		copy(context[16:32], []byte{0x93, 0xad, 0x25, 0x50, 0x9c, 0xb4, 0x11, 0xe7, 0xb4, 0x23, 0x83, 0xde, 0x96, 0x8b, 0xcd, 0x7c})
		binary.LittleEndian.PutUint32(context[32:], 0o644)
		binary.LittleEndian.PutUint32(body[48:], uint32(offset))
		binary.LittleEndian.PutUint32(body[52:], uint32(len(context)))
		r := c.exchange(5, append(body, context...))
		c.expectStatus(r, wireSuccess)
		if len(r.body) < 88 {
			t.Fatal("short POSIX CREATE response")
		}
		var id wireID
		copy(id[:], r.body[64:80])
		return id
	}
	for _, tc := range []struct {
		name string
		kind uint32
	}{
		{name: "", kind: 1},
		{name: "file", kind: 0},
		{name: "dir/script", kind: 0},
		{name: "link", kind: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := create(tc.name, 1, 0, 0x00120089)
			data, r := c.queryInfo(id, 100)
			c.expectStatus(r, wireSuccess)
			if len(data) < 80 {
				t.Fatal("short POSIX QUERY_INFO payload")
			}
			mode := binary.LittleEndian.Uint32(data[76:])
			// The served view's mode: the root is the upper's, with Windows' default mode.
			info, err := fixture.fs.Lstat(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if mode&0x7000 != tc.kind<<12 || mode&0o777 != uint32(info.Mode().Perm()) {
				t.Fatalf("POSIX mode %#o, want type %d and permissions %#o", mode, tc.kind, info.Mode().Perm())
			}
			_, r = c.queryInfo(id, 15)
			c.expectStatus(r, 0xc0000052)
			if tc.kind == 2 {
				_, r = c.ioctl(id, 0x000900a8, nil, 4096)
				c.expectStatus(r, wireSuccess)
			}
			c.expectStatus(c.close(id), wireSuccess)
		})
	}
	id := create("attribute-link", 2, 0x200040, 0x00110180)
	target := wireUTF16("file")
	reparse := make([]byte, 20)
	binary.LittleEndian.PutUint32(reparse, 0xa000000c)
	binary.LittleEndian.PutUint16(reparse[4:], uint16(12+2*len(target)))
	binary.LittleEndian.PutUint16(reparse[10:], uint16(len(target)))
	binary.LittleEndian.PutUint16(reparse[12:], uint16(len(target)))
	binary.LittleEndian.PutUint16(reparse[14:], uint16(len(target)))
	binary.LittleEndian.PutUint32(reparse[16:], 1)
	reparse = append(reparse, target...)
	reparse = append(reparse, target...)
	_, r := c.ioctl(id, 0x000900a4, reparse, 0)
	c.expectStatus(r, wireSuccess)
	c.expectStatus(c.close(id), wireSuccess)
	if target, err := os.Readlink(filepath.Join(fixture.upper, "attribute-link")); err != nil || target != "file" {
		t.Fatalf("attribute-only reparse target = %q, %v", target, err)
	}
}

// TestSMBNTLMv2Signing verifies both SMB3 KDFs, response signatures, credential failure, and tamper rejection.
func TestSMBNTLMv2Signing(t *testing.T) {
	fixture := startSMB(t, map[string]string{"alice": "correct horse battery staple"}, nil)
	for _, dialect := range []uint16{0x0302, 0x0311} {
		t.Run(fmt.Sprintf("dialect-%04x", dialect), func(t *testing.T) {
			c := newWireClient(t, fixture.address)
			c.negotiate([]uint16{dialect})
			c.expectStatus(c.login("alice", "correct horse battery staple", true), wireSuccess)
			if len(c.key) != 16 {
				t.Fatal("authenticated session did not produce a signing key")
			}
			c.expectStatus(c.treeConnect("test"), wireSuccess)
			name := fmt.Sprintf("signed-%04x", dialect)
			id := mustOpen(t, c, name, 2, 0x40)
			c.expectStatus(c.write(id, 0, []byte("signed content")), wireSuccess)
			readEquals(t, c, id, "signed content")
			c.expectStatus(c.close(id), wireSuccess)
			if c.verified < 5 {
				t.Fatalf("only %d signed responses verified", c.verified)
			}
			body := []byte{4, 0, 0, 0}
			request := c.packet(13, body, 0, c.tree, c.session)
			c.sign(request)
			request[48] ^= 0x80
			if err := c.writeFrame(request); err != nil {
				t.Fatal(err)
			}
			raw, err := c.readFrame()
			if err == nil {
				r, err := decodeWireReply(raw)
				if err != nil {
					t.Fatal(err)
				}
				c.expectStatus(r, wireAccessDenied)
			} else {
				var timeout net.Error
				if errors.As(err, &timeout) && timeout.Timeout() {
					t.Fatal("tampered signature neither rejected nor disconnected")
				}
				if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
					t.Fatalf("tamper rejection read: %v", err)
				}
			}
			wrong := newWireClient(t, fixture.address)
			wrong.negotiate([]uint16{dialect})
			wrong.expectStatus(wrong.login("alice", "wrong password", false), wireLogonFailure)
		})
	}
}

// TestSMBSymlinkReparsePoint round-trips a relative symlink through FSCTL reparse buffers.
func TestSMBSymlinkReparsePoint(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "target", "target data") })
	c := guestClient(t, fixture)
	id := mustOpen(t, c, "link", 2, 0x200040)
	target := wireUTF16("target")
	reparse := make([]byte, 20)
	binary.LittleEndian.PutUint32(reparse, 0xa000000c)
	binary.LittleEndian.PutUint16(reparse[4:], uint16(12+2*len(target)))
	binary.LittleEndian.PutUint16(reparse[10:], uint16(len(target)))
	binary.LittleEndian.PutUint16(reparse[12:], uint16(len(target)))
	binary.LittleEndian.PutUint16(reparse[14:], uint16(len(target)))
	binary.LittleEndian.PutUint32(reparse[16:], 1)
	reparse = append(reparse, target...)
	reparse = append(reparse, target...)
	_, r := c.ioctl(id, 0x000900a4, reparse, 0)
	c.expectStatus(r, wireSuccess)
	data, r := c.ioctl(id, 0x000900a8, nil, 4096)
	c.expectStatus(r, wireSuccess)
	if len(data) < 20 || binary.LittleEndian.Uint32(data) != 0xa000000c {
		t.Fatal("GET_REPARSE_POINT did not return a symlink buffer")
	}
	off, size := int(binary.LittleEndian.Uint16(data[8:])), int(binary.LittleEndian.Uint16(data[10:]))
	if off > len(data)-20 || size > len(data)-20-off {
		t.Fatal("invalid reparse target bounds")
	}
	if got := wireString(data[20+off : 20+off+size]); got != "target" {
		t.Fatalf("symlink target %q", got)
	}
	if binary.LittleEndian.Uint32(data[16:])&1 == 0 {
		t.Fatal("relative symlink flag was lost")
	}
	c.expectStatus(c.close(id), wireSuccess)
	if got, err := os.Readlink(filepath.Join(fixture.upper, "link")); err != nil || got != "target" {
		t.Fatalf("upper symlink = %q, %v", got, err)
	}
	reopened := mustOpen(t, c, "link", 1, 0x200040)
	data, r = c.ioctl(reopened, 0x000900a8, nil, 4096)
	c.expectStatus(r, wireSuccess)
	if !bytes.Equal(data, reparse) {
		t.Fatal("reopening the reparse point changed its target")
	}
	c.expectStatus(c.close(reopened), wireSuccess)
	opened := mustOpen(t, c, "link", 1, 0x40)
	got, r := c.read(opened, 0, 64)
	c.expectStatus(r, wireSuccess)
	if !bytes.Equal(got, []byte("target data")) {
		t.Fatalf("symlink read = %q", got)
	}
	c.expectStatus(c.close(opened), wireSuccess)
	if got, err := os.Readlink(filepath.Join(fixture.upper, "link")); err != nil || got != "target" {
		t.Fatalf("normal open replaced the symlink: %q, %v", got, err)
	}
	for _, tc := range []struct {
		name, target, open string
	}{
		{name: "parent-relative", target: "../target", open: "dir/link"},
		{name: "chain", target: "../alias", open: "dir/link"},
		{name: "directory-component", target: "../dir", open: "dir/link/child"},
		{name: "cycle", target: "link", open: "dir/link"},
		{name: "absolute", open: "dir/link"}, // the lower file's host path, a drive path on Windows
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := startSMB(t, nil, func(lower, upper string) {
				putDiskFile(t, lower, "target", "target data")
				putDiskFile(t, lower, "dir/child", "target data")
				if err := os.Symlink("target", filepath.Join(lower, "alias")); err != nil {
					t.Fatal(err)
				}
				putDiskFile(t, upper, "dir/placeholder", "")
				target := tc.target
				if target == "" {
					target = filepath.Join(lower, "target")
				}
				if err := os.Symlink(target, filepath.Join(upper, "dir/link")); err != nil {
					t.Fatal(err)
				}
			})
			c := guestClient(t, fixture)
			if tc.name == "cycle" {
				_, r := c.create(tc.open, 1, 0x40)
				c.expectStatus(r, statusTooManyLinks)
				id := mustOpen(t, c, tc.open, 1, 0x200040)
				_, r = c.ioctl(id, 0x000900a8, nil, 4096)
				c.expectStatus(r, wireSuccess)
				c.expectStatus(c.close(id), wireSuccess)
				return
			}
			id := mustOpen(t, c, tc.open, 1, 0x40)
			readEquals(t, c, id, "target data")
			c.expectStatus(c.close(id), wireSuccess)
		})
	}
}

// createShared opens an existing name with explicit access and share masks.
func createShared(c *wireClient, name string, options, access, share uint32) (wireID, wireReply) {
	c.t.Helper()
	b := wireCreateBody(name, 1, options)
	binary.LittleEndian.PutUint32(b[24:], access)
	binary.LittleEndian.PutUint32(b[32:], share)
	r := c.exchange(5, b)
	var id wireID
	if r.status == wireSuccess {
		copy(id[:], r.body[64:80])
	}
	return id, r
}

// TestSMBAttributeOnlyOpensTakeNoPartInSharing keeps sharing to opens with
// read, write, execute or delete access, as NTFS does: Windows holds the share
// root open for free-space queries with no sharing, and listing it must still
// work, while an unshared data open still locks readers out.
func TestSMBAttributeOnlyOpensTakeNoPartInSharing(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "dir/file", "data") })
	c := guestClient(t, fixture)
	// FILE_OPEN_FOR_FREE_SPACE_QUERY with READ_ATTRIBUTES|SYNCHRONIZE and no sharing.
	query, r := createShared(c, "dir", 0x800021, 0x100080, 0)
	c.expectStatus(r, wireSuccess)
	list, r := createShared(c, "dir", 0x21, 0x100081, 7)
	c.expectStatus(r, wireSuccess)
	c.expectStatus(c.close(list), wireSuccess)
	c.expectStatus(c.close(query), wireSuccess)

	data, r := createShared(c, "dir/file", 0x40, 0x100001, 0)
	c.expectStatus(r, wireSuccess)
	_, r = createShared(c, "dir/file", 0x40, 0x100001, 7)
	c.expectStatus(r, statusSharingViolation)
	attrs, r := createShared(c, "dir/file", 0x40, 0x100080, 0)
	c.expectStatus(r, wireSuccess)
	c.expectStatus(c.close(attrs), wireSuccess)
	c.expectStatus(c.close(data), wireSuccess)
}

// junctionReparse encodes an IO_REPARSE_TAG_MOUNT_POINT buffer for target.
func junctionReparse(target string) []byte {
	sub := wireUTF16(`\??\` + target)
	prn := wireUTF16(target)
	b := make([]byte, 16, 16+len(sub)+len(prn)+4)
	binary.LittleEndian.PutUint32(b, ntinfo.ReparseTagMountPoint)
	binary.LittleEndian.PutUint16(b[4:], uint16(8+len(sub)+len(prn)+4))
	binary.LittleEndian.PutUint16(b[10:], uint16(len(sub)))
	binary.LittleEndian.PutUint16(b[12:], uint16(len(sub)+2))
	binary.LittleEndian.PutUint16(b[14:], uint16(len(prn)))
	b = append(append(append(b, sub...), 0, 0), prn...)
	return append(b, 0, 0)
}

// TestSMBDirectoryLinks turns the empty directory CreateSymbolicLink (with
// SYMBOLIC_LINK_FLAG_DIRECTORY) or a junction tool creates into a symlink, as
// bun install and mklink /D do, and refuses what NTFS refuses.
func TestSMBDirectoryLinks(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, lower, "packages/utils/child", "child data") })
	c := guestClient(t, fixture)
	link := func(name string, reparse []byte, want uint32) {
		t.Helper()
		id := mustOpen(t, c, name, 2, 0x200001)
		_, r := c.ioctl(id, 0x000900a4, reparse, 0)
		c.expectStatus(r, want)
		c.expectStatus(c.close(id), wireSuccess)
	}
	symlink, err := ntinfo.SymlinkReparse("packages/utils")
	if err != nil {
		t.Fatal(err)
	}
	link("dlink", symlink, wireSuccess)
	if got, err := os.Readlink(filepath.Join(fixture.upper, "dlink")); err != nil || filepath.ToSlash(got) != "packages/utils" {
		t.Fatalf("directory symlink = %q, %v", got, err)
	}
	id := mustOpen(t, c, "dlink/child", 1, 0x40)
	readEquals(t, c, id, "child data")
	c.expectStatus(c.close(id), wireSuccess)

	abs := filepath.Join(fixture.lower, "packages", "utils")
	link("jlink", junctionReparse(abs), wireSuccess)
	if got, err := os.Readlink(filepath.Join(fixture.upper, "jlink")); err != nil || got != abs {
		t.Fatalf("junction stored as %q, %v; want a symlink to %q", got, err, abs)
	}

	full := mustOpen(t, c, "full", 2, 0x200001)
	c.expectStatus(c.close(mustOpen(t, c, "full/x", 2, 0x40)), wireSuccess)
	_, r := c.ioctl(full, 0x000900a4, symlink, 0)
	c.expectStatus(r, statusDirectoryNotEmpty)
	c.expectStatus(c.close(full), wireSuccess)

	file := mustOpen(t, c, "file", 2, 0x200040)
	_, r = c.ioctl(file, 0x000900a4, junctionReparse(abs), 0)
	c.expectStatus(r, statusNotADirectory)
	c.expectStatus(c.close(file), wireSuccess)
}

// TestSMBMountRelativeLinks stores a link whose target names the link's own
// checkout through one of the server's mount roots relative to the link, so
// it resolves inside the checkout; a target in another checkout stays as
// written, since no relative path reaches it.
func TestSMBMountRelativeLinks(t *testing.T) {
	fixture := startSMBWith(t, func(s *Server) { s.MountRoots = []string{"O:/", "//127.0.0.1/ovnfs/"} }, func(lower, upper string) {
		putDiskFile(t, lower, "co/packages/utils/child", "child data")
	})
	c := guestClient(t, fixture)
	for _, tc := range []struct{ link, target, want string }{
		{"co/drive", "O:/co/packages/utils", "packages/utils"},
		{"co/share", "//127.0.0.1/OVNFS/co/packages/utils", "packages/utils"},
		{"co/other", "O:/other/x", "O:/other/x"},
	} {
		reparse, err := ntinfo.SymlinkReparse(tc.target)
		if err != nil {
			t.Fatal(err)
		}
		id := mustOpen(t, c, tc.link, 2, 0x200001)
		_, r := c.ioctl(id, 0x000900a4, reparse, 0)
		c.expectStatus(r, wireSuccess)
		c.expectStatus(c.close(id), wireSuccess)
		if got, err := os.Readlink(filepath.Join(fixture.upper, tc.link)); err != nil || filepath.ToSlash(got) != tc.want {
			t.Errorf("%s stored as %q, %v; want %q", tc.link, got, err, tc.want)
		}
	}
	id := mustOpen(t, c, "co/share/child", 1, 0x40)
	readEquals(t, c, id, "child data")
	c.expectStatus(c.close(id), wireSuccess)
}
