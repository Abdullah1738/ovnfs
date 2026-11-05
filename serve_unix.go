//go:build !windows

package main

import (
	"log"
	"net"
	"os"
	"syscall"

	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/nfs"
)

// serveNFS serves NFSv3 and NFSv4 on the persistent export. NFSv4 clients
// get read delegations, recalled whenever the export reports a change made
// behind their back.
func serveNFS(l net.Listener, e *export.Export) error {
	srv := &nfs.Server{FS: e.NFS(), Concurrency: 64}
	if os.Getenv("LOG_LEVEL") != "" {
		srv.Logf = log.Printf
	}
	e.OnRecall(srv.Recall)
	return srv.Serve(l)
}

// refreshRoot makes the local NFS client revalidate the export root now
// rather than after its attribute timeout: the FSSTAT reply carries the
// root's attributes, and its changed mtime purges the client's cached names,
// including a negative entry for a checkout created a moment ago. The caller
// must hold no registry lock, since the server answers the FSSTAT.
func refreshRoot(mount string) {
	var st syscall.Statfs_t
	_ = syscall.Statfs(mount, &st)
}
