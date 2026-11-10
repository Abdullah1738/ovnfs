package main

import (
	"context"
	"github.com/stencil-hq/ovnfs/checkout"
	"path/filepath"
)

func daemonHolder(ctx context.Context, state string) (int, bool, error) {
	state, err := filepath.Abs(state)
	if err != nil {
		return 0, false, err
	}
	pid, held, err := checkout.StateHolder(state)
	if err != nil || !held {
		return 0, held, err
	}
	if pid > 0 && processOwnsState(ctx, pid, state) {
		return pid, true, nil
	}
	return legacyStateHolder(ctx, state), true, nil
}

// Spawned serves always carry an absolute state argument. Accept the CLI's
// separate and equals forms, but never infer ownership from a path substring.
func servesState(args []string, state string) bool {
	if len(args) < 2 {
		return false
	}
	var c cli
	rest, err := splitGlobals(&c, args[1:])
	return err == nil && len(rest) > 0 && rest[0] == "serve" && filepath.IsAbs(c.state) && filepath.Clean(c.state) == filepath.Clean(state)
}
