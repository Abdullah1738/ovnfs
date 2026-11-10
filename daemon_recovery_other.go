//go:build !darwin

package main

import (
	"context"
	"fmt"
	"os"
)

func detachStaleMount(ctx context.Context, state, mount string) error {
	if mounted(mount) {
		return fmt.Errorf("daemon stopped; stale mount %s needs recovery before starting", mount)
	}
	return nil
}

func recoverySupported() bool { return false }

func finishRecovery(context.Context, string, string) error { return nil }

func releaseMount(_ string, mount string, force bool) error { return unmount(mount, force) }

func lockLegacyHandoff(context.Context, string, string) (*os.File, error) { return nil, nil }

func finishHandoff(context.Context, string, string) error { return nil }
