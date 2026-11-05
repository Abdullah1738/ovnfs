package fuse

import (
	"fmt"

	"github.com/stencil-hq/ovnfs/export"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
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
