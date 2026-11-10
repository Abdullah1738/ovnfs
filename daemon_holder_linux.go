//go:build linux

package main

import (
	"context"
	"github.com/stencil-hq/ovnfs/checkout"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processOwnsState(ctx context.Context, pid int, state string) bool {
	owner, held, err := checkout.StateHolder(state)
	if err != nil || !held || owner != pid {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	return err == nil && servesState(strings.Split(string(data), "\x00"), state)
}

func legacyStateHolder(ctx context.Context, state string) int { return 0 }
