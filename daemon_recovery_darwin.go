//go:build darwin

package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func detachStaleMount(ctx context.Context, state, mount string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := lockUnmount(ctx, state)
	if err != nil {
		return err
	}
	defer lease.Close()
	return detachStaleMountLocked(ctx, state, mount)
}

func detachStaleMountLocked(ctx context.Context, state, mount string) error {
	entry, found, err := mountRecord(mount)
	if err != nil || !found {
		return err
	}
	if unix.ByteSliceToString(entry.Fstypename[:]) != "nfs" || unix.ByteSliceToString(entry.Mntfromname[:]) != "<"+nfsSocket(state)+">:/" {
		return fmt.Errorf("refusing recovery unmount of %s: it is not this state's NFS socket mount", mount)
	}
	appendDaemonLog(state, "recovery force-unmount of "+mount+"; outstanding I/O may fail")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return trackedUnmountLocked(ctx, state, mount, true)
}

func recoverySupported() bool { return true }
