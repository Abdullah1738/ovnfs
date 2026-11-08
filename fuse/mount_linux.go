package fuse

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stencil-hq/ovnfs/export"
	"golang.org/x/sys/unix"
)

// Server is an active FUSE mount of the export root.
type Server struct {
	srv        *gofuse.Server
	mountpoint string
}

// Mount serves e at mountpoint until Unmount, creating the directory. A dead
// mount left there by a server that was killed before it unmounted (kill -9,
// the OOM killer) is detached first, since it fails both mkdir and a new
// mount. It mounts through fusermount3 (or fusermount), so it needs no root
// where the distribution allows unprivileged FUSE mounts.
func Mount(e *export.Export, mountpoint string, opts Options) (*Server, error) {
	if err := detachDead(mountpoint); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return nil, fmt.Errorf("fuse: mount directory: %w", err)
	}
	fsName := opts.FsName
	if fsName == "" {
		fsName = "ovnfs"
	}
	srv, err := gofuse.NewServer(newExportFS(e.Nodes()), mountpoint, &gofuse.MountOptions{
		AllowOther: opts.AllowOther,
		FsName:     fsName,
		Name:       "ovnfs",
		// The kernel checks modes and owners itself; nodev and nosuid are
		// what fusermount forces on unprivileged mounts anyway.
		Options:  []string{"default_permissions", "nodev", "nosuid", "noatime"},
		MaxWrite: gofuse.MAX_KERNEL_WRITE,
		Debug:    opts.Debug,
	})
	if err != nil {
		return nil, fmt.Errorf("fuse: mount %s: %w", mountpoint, err)
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		srv.Unmount()
		return nil, fmt.Errorf("fuse: mount %s: %w", mountpoint, err)
	}
	return &Server{srv: srv, mountpoint: mountpoint}, nil
}

// detachDead lazily unmounts mountpoint when it is a FUSE mount whose server
// is gone, which the kernel answers with ENOTCONN; anything else, a live mount
// included, is left alone. Lazily, because processes may still hold the dead
// mount as their working directory.
func detachDead(mountpoint string) error {
	// Forced: a plain stat is answered from attributes the kernel still caches
	// for up to TTL after the server died.
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, mountpoint, unix.AT_STATX_FORCE_SYNC, unix.STATX_TYPE, &stx); !errors.Is(err, unix.ENOTCONN) {
		return nil
	}
	bin, err := exec.LookPath("fusermount3")
	if err != nil {
		if bin, err = exec.LookPath("fusermount"); err != nil {
			return fmt.Errorf("fuse: detach dead mount %s: %w", mountpoint, err)
		}
	}
	if out, err := exec.Command(bin, "-u", "-z", mountpoint).CombinedOutput(); err != nil {
		return fmt.Errorf("fuse: detach dead mount %s: %w: %s", mountpoint, err, bytes.TrimSpace(out))
	}
	return nil
}

// Unmount detaches the mount; it fails while files on it are in use.
func (s *Server) Unmount() error {
	if err := s.srv.Unmount(); err != nil {
		return fmt.Errorf("fuse: unmount %s: %w", s.mountpoint, err)
	}
	return nil
}

// Wait blocks until the mount is gone, by Unmount or an external umount.
func (s *Server) Wait() { s.srv.Wait() }
