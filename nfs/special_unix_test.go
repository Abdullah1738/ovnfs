//go:build !windows

package nfs

import (
	"os"
	"testing"

	"github.com/stencil-hq/ovnfs/overlay"
)

type specialFS struct {
	*testFS
	upper *overlay.FS
}

func (f *specialFS) Mknod(name string, mode os.FileMode) error { return f.upper.Mknod(name, mode) }

func TestV3SpecialNodes(t *testing.T) {
	base := newTestFS(t)
	upper, err := overlay.New(base.root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &specialFS{base, upper}
	c := v3server(t, f)
	for _, tc := range []struct {
		name string
		typ  fileType
		mode os.FileMode
	}{{"socket", typeSock, os.ModeSocket}, {"fifo", typeFifo, os.ModeNamedPipe}} {
		st, _ := c.v3call(v3Mknod, func(e *encoder) { dirop(e, 1, tc.name); e.u32(uint32(tc.typ)); noSattr(e) })
		if st != stOK {
			t.Fatalf("MKNOD %s: %d", tc.name, st)
		}
		info, err := os.Lstat(base.host(tc.name))
		if err != nil || info.Mode()&os.ModeType != tc.mode {
			t.Fatalf("%s: %v, %v", tc.name, info, err)
		}
	}
}

func TestV4SpecialNodes(t *testing.T) {
	base := newTestFS(t)
	upper, err := overlay.New(base.root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &specialFS{base, upper}
	_, addr := v4serve(t, f, nil)
	c := v4dial(t, addr)
	for _, tc := range []struct {
		name string
		typ  fileType
		mode os.FileMode
	}{{"socket", typeSock, os.ModeSocket}, {"fifo", typeFifo, os.ModeNamedPipe}} {
		if st := c.run(v4Root(), v4Create(tc.typ, tc.name, "", v4fattr{})).status; st != stOK {
			t.Fatalf("CREATE %s: %d", tc.name, st)
		}
		info, err := os.Lstat(base.host(tc.name))
		if err != nil || info.Mode()&os.ModeType != tc.mode {
			t.Fatalf("%s: %v, %v", tc.name, info, err)
		}
	}
}
