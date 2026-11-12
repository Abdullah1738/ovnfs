//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/stencil-hq/ovnfs/checkout"
	"golang.org/x/sys/unix"
)

func processOwnsState(ctx context.Context, pid int, state string) bool {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(data) < 4 {
		return false
	}
	argc := int(binary.LittleEndian.Uint32(data[:4]))
	data = data[4:]
	i := bytes.IndexByte(data, 0)
	if i < 0 {
		return false
	}
	data = bytes.TrimLeft(data[i+1:], "\x00")
	var args []string
	for len(args) < argc && len(data) > 0 {
		i = bytes.IndexByte(data, 0)
		if i < 0 {
			return false
		}
		args = append(args, string(data[:i]))
		data = data[i+1:]
	}
	if !servesState(args, state) {
		return false
	}
	owner, held, err := checkout.StateHolder(state)
	return err == nil && held && owner == pid
}

// Old flock-only holders cannot be associated with a pid safely. Keep their
// state excluded, rather than using stale text or walking their NFS descriptors.
func legacyStateHolder(context.Context, string) int { return 0 }
