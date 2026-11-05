//go:build windows

package main

import (
	"fmt"
	"github.com/stencil-hq/ovnfs/export"
	"net"
)

// serveNFS rejects a protocol unavailable on Windows.
func serveNFS(net.Listener, *export.Export) error {
	return fmt.Errorf("NFS serving unsupported on Windows; use -proto smb")
}

// refreshRoot has no NFS client cache to refresh on Windows.
func refreshRoot(string) {}
