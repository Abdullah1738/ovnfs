package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

func appendDaemonLog(state, message string) {
	f, err := os.OpenFile(logPath(state), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006/01/02 15:04:05"), message)
}

// Recovery is distinct from a planned handoff: a forced unmount invalidates
// outstanding client I/O. Never use it merely because startup exceeded 15s.
func recoverDaemon(ctx context.Context, state, mount string, pid int) error {
	for _, force := range []bool{false, true} {
		if err := ctx.Err(); err != nil {
			return err
		}
		owner, held, err := daemonHolder(ctx, state)
		if err != nil {
			return err
		}
		if !held {
			break
		}
		if owner != pid || pid <= 0 {
			return fmt.Errorf("state lock owner changed from pid %d to %d; recovery refused", pid, owner)
		}
		appendDaemonLog(state, fmt.Sprintf("recovery requested by cli pid %d: holder %d force=%t", os.Getpid(), pid, force))
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := signalDaemon(pid, force); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			owner, held, err = daemonHolder(ctx, state)
			if err != nil {
				return err
			}
			if !held {
				break
			}
			if owner > 0 && owner != pid {
				return fmt.Errorf("state lock changed owner during recovery; refusing to continue")
			}
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		if !held {
			break
		}
	}
	if _, held, err := daemonHolder(ctx, state); err != nil || held {
		return fmt.Errorf("state lock still held after recovery of pid %d: %v", pid, err)
	}
	if err := finishRecovery(ctx, state, mount); err != nil {
		return err
	}
	return detachStaleMount(ctx, state, mount)
}
