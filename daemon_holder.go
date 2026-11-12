package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/stencil-hq/ovnfs/checkout"
	"io"
	"os"
	"path/filepath"
)

func daemonHolder(ctx context.Context, state string) (int, bool, error) {
	state, err := filepath.Abs(state)
	if err != nil {
		return 0, false, err
	}
	pid, held, err := checkout.StateHolder(state)
	if err != nil {
		return 0, held, fmt.Errorf("can't read state: %s: %w", state, err)
	}
	if !held {
		if pid, held, err := pendingStartupHolder(ctx, state); err != nil || held {
			return pid, held, err
		}
		return savedPortHolder(state)
	}

	if pid > 0 && processOwnsState(ctx, pid, state) {
		return pid, true, nil
	}
	if legacy := legacyStateHolder(ctx, state); legacy > 0 {
		return legacy, true, nil
	}
	if portPID, busy, err := savedPortHolder(state); err == nil && busy {
		return portPID, true, nil
	}
	return 0, true, nil
}

// Spawned serves always carry an absolute state argument. Accept the CLI's
// separate and equals forms, but never infer ownership from a path substring.
func servesState(args []string, state string) bool {
	if len(args) < 2 {
		return false
	}
	var c cli
	rest, err := splitGlobals(&c, args[1:])
	return err == nil && len(rest) > 0 && rest[0] == "serve" && filepath.IsAbs(c.state) && sameStateDirectory(c.state, state)
}

// State paths can differ in case, extended-prefix spelling or junction/symlink
// aliases. Compare the directory identity before concluding a serve is unrelated.
func sameStateDirectory(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && left.IsDir() && right.IsDir() && os.SameFile(left, right)
}

// Auto-start uses only these two private serve flags after global options.
// Foreground serves cannot progress while a different starter owns start.lock.
func borrowsStartForState(args []string, state string) bool {
	if !servesState(args, state) {
		return false
	}
	var c cli
	rest, err := splitGlobals(&c, args[1:])
	if err != nil || len(rest) == 0 {
		return false
	}
	fs := flag.NewFlagSet("spawned serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	parent, fd := 0, 0
	fs.IntVar(&parent, "starter-pid", 0, "")
	fs.IntVar(&fd, "starter-fd", 0, "")
	return fs.Parse(rest[1:]) == nil && fs.NArg() == 0 && parent > 0 && fd == 0
}
