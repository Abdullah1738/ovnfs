package fuse

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stencil-hq/ovnfs/export"
)

// Server is an active FUSE mount of the export root.
type Server struct {
	srv        *gofuse.Server
	mountpoint string
}

// Mount serves e at the existing directory mountpoint until Unmount. It
// mounts through fusermount3 (or fusermount), so it needs no root where the
// distribution allows unprivileged FUSE mounts.
func Mount(e *export.Export, mountpoint string, opts Options) (*Server, error) {
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

// Unmount detaches the mount; it fails while files on it are in use.
func (s *Server) Unmount() error {
	if err := s.srv.Unmount(); err != nil {
		return fmt.Errorf("fuse: unmount %s: %w", s.mountpoint, err)
	}
	return nil
}

// Wait blocks until the mount is gone, by Unmount or an external umount.
func (s *Server) Wait() { s.srv.Wait() }

// ClearStale detaches a dead FUSE mount left at mountpoint by a daemon that
// exited without unmounting (killed, or crashed). Such a mount answers every
// call with ENOTCONN, so the mount point can be neither created nor mounted
// over until it is detached. It asks with statfs, which the kernel always
// sends to the server: stat can be answered from the attribute cache for a
// second after the server died. A lazy unmount detaches it at once; processes
// still holding a path below it keep a dead handle. A live mount, whoever
// serves it, is left alone.
func ClearStale(mountpoint string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &st); !errors.Is(err, syscall.ENOTCONN) {
		return nil
	}
	var errs []error
	for _, helper := range []string{"fusermount3", "fusermount"} {
		out, err := exec.Command(helper, "-u", "-z", mountpoint).CombinedOutput()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w: %s", helper, err, strings.TrimSpace(string(out))))
	}
	return fmt.Errorf("fuse: detach stale mount %s: %w", mountpoint, errors.Join(errs...))
}
