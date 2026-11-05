// Package fuse serves the export root over the Linux kernel's FUSE protocol:
// the root lists checkouts read-only and each checkout below it is fully writable.
package fuse

import "time"

// TTL bounds how long the kernel caches entries and attributes; it is short
// because a checkout's overlay changes underneath on snapshot, fork, and removal.
const TTL = time.Second

// Options tunes one FUSE mount.
type Options struct {
	// AllowOther lets users other than the mounting one access the mount;
	// unprivileged mounts need user_allow_other in /etc/fuse.conf.
	AllowOther bool
	// FsName is the mount source shown in /proc/mounts; empty means "ovnfs".
	FsName string
	// Debug logs every FUSE request and reply.
	Debug bool
}
