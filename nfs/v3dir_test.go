package nfs

import (
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
)

// dirEntry3 is a decoded entry3 or entryplus3.
type dirEntry3 struct {
	fileid, cookie uint64
	name           string
	attrs          *wireAttr
	handle         uint64
	hasHandle      bool
}

// dirPage3 is a decoded READDIR or READDIRPLUS result.
type dirPage3 struct {
	st      status
	size    int // bytes of the result, from the status on
	verf    uint64
	entries []dirEntry3
	eof     bool
	failLen int // failure body bytes
}

// dirCaller sends one READDIR or READDIRPLUS and returns its accept_stat
// and result.
type dirCaller func(proc uint32, args func(*encoder)) (acceptStat, *decoder)

// readdir3 calls READDIR (dircount 0) or READDIRPLUS of dir and decodes it.
func readdir3(t *testing.T, call dirCaller, plus bool, dir, cookie, verf uint64, dircount, maxcount uint32) dirPage3 {
	t.Helper()
	proc := uint32(v3ReadDir)
	if plus {
		proc = v3ReadDirPlus
	}
	stat, d := call(proc, func(e *encoder) {
		e.fh(dir)
		e.u64(cookie)
		e.u64(verf)
		if plus {
			e.u32(dircount)
		}
		e.u32(maxcount)
	})
	if stat != rpcSuccess {
		t.Fatalf("accept_stat %d", stat)
	}
	pg := dirPage3{size: len(d.buf)}
	pg.st = status(d.u32())
	if pg.st != stOK {
		pg.failLen = len(d.buf)
		return pg
	}
	if _, ok := postOpOf(t, d); !ok {
		t.Fatal("no directory attributes")
	}
	pg.verf = d.u64()
	for d.bool() {
		e := dirEntry3{fileid: d.u64(), name: d.str(maxPath3), cookie: d.u64()}
		if plus {
			if a, ok := postOpOf(t, d); ok {
				e.attrs = &a
			}
			if e.hasHandle = d.bool(); e.hasHandle {
				e.handle, _ = d.fh(maxFH3)
			}
		}
		pg.entries = append(pg.entries, e)
	}
	pg.eof = d.bool()
	if d.err != nil || len(d.buf) != 0 {
		t.Fatal("malformed result")
	}
	return pg
}

// wire calls over a client.
func wire(c *rpcClient) dirCaller {
	return func(proc uint32, args func(*encoder)) (acceptStat, *decoder) {
		return c.call(progNFS, 3, proc, args)
	}
}

// direct calls s.nfs3 in-process, so a test can drop s's cached listings
// between calls without racing the server.
func direct(s *Server) dirCaller {
	return func(proc uint32, args func(*encoder)) (acceptStat, *decoder) {
		var a encoder
		args(&a)
		c := &call{prog: progNFS, vers: 3, proc: proc, args: decoder{buf: a.buf}}
		var res encoder
		stat := s.nfs3(c, &res)
		return stat, &decoder{buf: res.buf}
	}
}

// dirFixture makes a directory "d" of files with names of varied length and
// returns their sorted names.
func dirFixture(t *testing.T, fs *testFS, n int) []string {
	t.Helper()
	if err := os.Mkdir(fs.host("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range n {
		name := fmt.Sprintf("f%02d-%s", i, strings.Repeat("x", i%7))
		fs.write(t, path.Join("d", name), strings.Repeat("y", i))
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// pageAll lists dir to the end and returns the names seen; every page must
// keep within its budgets and carry the first page's verifier.
func pageAll(t *testing.T, call dirCaller, plus bool, dir uint64, dircount, maxcount uint32) []dirEntry3 {
	t.Helper()
	var all []dirEntry3
	var cookie, verf uint64
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("listing does not end")
		}
		pg := readdir3(t, call, plus, dir, cookie, verf, dircount, maxcount)
		if pg.st != stOK {
			t.Fatalf("page %d: status %d", pages, pg.st)
		}
		if pg.size > int(maxcount) {
			t.Fatalf("page %d: %d bytes, budget %d", pages, pg.size, maxcount)
		}
		if plus {
			used := 0
			for _, e := range pg.entries {
				used += 20 + len(e.name) + pad(len(e.name))
			}
			if used > int(dircount) {
				t.Fatalf("page %d: %d directory bytes, dircount %d", pages, used, dircount)
			}
		}
		if verf != 0 && pg.verf != verf {
			t.Fatalf("page %d: verifier changed", pages)
		}
		verf = pg.verf
		all = append(all, pg.entries...)
		if pg.eof {
			return all
		}
		if len(pg.entries) == 0 {
			t.Fatalf("page %d: no entries and no eof", pages)
		}
		cookie = pg.entries[len(pg.entries)-1].cookie
	}
}

// names lists entries' names, checking cookies increase.
func names(t *testing.T, entries []dirEntry3) []string {
	t.Helper()
	var out []string
	for i, e := range entries {
		if i > 0 && e.cookie <= entries[i-1].cookie {
			t.Fatalf("cookie %d after %d", e.cookie, entries[i-1].cookie)
		}
		out = append(out, e.name)
	}
	return out
}

// TestV3ReaddirPaging pages READDIR and READDIRPLUS through a directory
// across budgets: every entry appears once, in order, after "." and "..",
// each page within its byte budgets.
func TestV3ReaddirPaging(t *testing.T) {
	fs := newTestFS(t)
	want := append([]string{".", ".."}, dirFixture(t, fs, 60)...)
	c := v3server(t, fs)
	dir := fs.FileID("d")
	for _, b := range []struct{ dircount, maxcount uint32 }{
		{4096, 140}, {4096, 300}, {60, 8192}, {512, 1024}, {1 << 20, 1 << 20},
	} {
		got := names(t, pageAll(t, wire(c), false, dir, 0, b.maxcount+150))
		if !slices.Equal(got, want) {
			t.Errorf("READDIR count %d: %v", b.maxcount+150, got)
		}
		plus := pageAll(t, wire(c), true, dir, b.dircount, b.maxcount+150)
		if got := names(t, plus); !slices.Equal(got, want) {
			t.Errorf("READDIRPLUS %d/%d: %v", b.dircount, b.maxcount+150, got)
		}
		for _, e := range plus {
			id := fs.FileID(path.Join("d", e.name))
			switch e.name {
			case ".":
				id = dir
			case "..":
				id = 1
			}
			if e.fileid != id || !e.hasHandle || e.handle != id || e.attrs == nil || e.attrs.fileid != id {
				t.Fatalf("entry %q: fileid %d handle %d (%v), attrs %+v; want %d", e.name, e.fileid, e.handle, e.hasHandle, e.attrs, id)
			}
		}
	}
}

// TestV3ReaddirCookies checks the cookies of "." and "..", a continuation
// from "." and a cookie the listing never issued.
func TestV3ReaddirCookies(t *testing.T) {
	fs := newTestFS(t)
	files := dirFixture(t, fs, 3)
	c := v3server(t, fs)
	dir := fs.FileID("d")

	first := readdir3(t, wire(c), false, dir, 0, 0, 0, 4096)
	var cookies []uint64
	for _, e := range first.entries {
		cookies = append(cookies, e.cookie)
	}
	if !first.eof || !slices.Equal(cookies, []uint64{1, 2, 3, 4, 5}) || first.entries[0].fileid != dir || first.entries[1].fileid != 1 {
		t.Fatalf("first page %+v", first)
	}
	if got := readdir3(t, wire(c), false, dir, 1, first.verf, 0, 4096); !slices.Equal(names(t, got.entries), append([]string{".."}, files...)) {
		t.Fatalf("after \".\": %v", names(t, got.entries))
	}
	if got := readdir3(t, wire(c), false, dir, 2, first.verf, 0, 4096); !slices.Equal(names(t, got.entries), files) {
		t.Fatalf("after \"..\": %v", names(t, got.entries))
	}
	if got := readdir3(t, wire(c), false, dir, 5, first.verf, 0, 4096); len(got.entries) != 0 || !got.eof {
		t.Fatalf("after the last entry: %+v", got)
	}
	if got := readdir3(t, wire(c), false, dir, 6, first.verf, 0, 4096); got.st != stBadCookie || got.failLen != 4 {
		t.Fatalf("cookie never issued: %d", got.st)
	}
	if got := readdir3(t, wire(c), false, dir, 0, 0, 0, 100); got.st != stTooSmall || got.failLen != 4 {
		t.Fatalf("count 100: %d, want TOOSMALL", got.st)
	}
	if got := readdir3(t, wire(c), true, dir, 0, 0, 10, 4096); got.st != stTooSmall {
		t.Fatalf("dircount 10: %d, want TOOSMALL", got.st)
	}
	if got := readdir3(t, wire(c), false, fs.FileID(path.Join("d", files[0])), 0, 0, 0, 4096); got.st != stNotDir || got.failLen != 4 {
		t.Fatalf("READDIR of a file: %d, want NOTDIR", got.st)
	}
}

// TestV3ReaddirChangesMidListing checks that a directory changing between
// pages neither skips nor repeats entries, that a continuation whose
// snapshot was evicted carries on while the directory is unchanged, and
// fails BAD_COOKIE once it changed. READDIRPLUS continuations describe
// entries as they are now, so they leave out one removed meanwhile.
func TestV3ReaddirChangesMidListing(t *testing.T) {
	fs := newTestFS(t)
	files := dirFixture(t, fs, 30)
	s := &Server{FS: fs}
	call := direct(s)
	dir := fs.FileID("d")

	for _, plus := range []bool{false, true} {
		first := readdir3(t, call, plus, dir, 0, 0, 4096, 600)
		if first.st != stOK || first.eof || len(first.entries) < 3 {
			t.Fatalf("first page: %+v", first)
		}
		seen := first.entries
		last := seen[len(seen)-1].name
		// Add an entry sorting first and remove one not listed yet.
		fs.write(t, "d/0-new", "")
		if err := os.Remove(fs.host(path.Join("d", files[len(files)-1]))); err != nil {
			t.Fatal(err)
		}
		cookie := seen[len(seen)-1].cookie
		for {
			pg := readdir3(t, call, plus, dir, cookie, first.verf, 4096, 600)
			if pg.st != stOK || pg.verf != first.verf {
				t.Fatalf("continuation after %q: %d", last, pg.st)
			}
			seen = append(seen, pg.entries...)
			if pg.eof {
				break
			}
			cookie = pg.entries[len(pg.entries)-1].cookie
		}
		want := append([]string{".", ".."}, files...)
		if plus {
			want = want[:len(want)-1]
		}
		if got := names(t, seen); !slices.Equal(got, want) {
			t.Fatalf("plus %v: listing across a change: %v", plus, got)
		}
		// Restore the directory for the next round.
		if err := os.Remove(fs.host("d/0-new")); err != nil {
			t.Fatal(err)
		}
		fs.write(t, path.Join("d", files[len(files)-1]), "")
	}

	first := readdir3(t, call, false, dir, 0, 0, 0, 400)
	cookie := first.entries[len(first.entries)-1].cookie
	s.dirs = dirCache{}
	if pg := readdir3(t, call, false, dir, cookie, first.verf, 0, 400); pg.st != stOK {
		t.Fatalf("evicted continuation of an unchanged directory: %d", pg.st)
	}
	s.dirs = dirCache{}
	fs.write(t, "d/zz-new", "")
	if pg := readdir3(t, call, true, dir, cookie, first.verf, 4096, 4096); pg.st != stBadCookie || pg.failLen != 4 {
		t.Fatalf("evicted continuation of a changed directory: %d, want BAD_COOKIE", pg.st)
	}
}
