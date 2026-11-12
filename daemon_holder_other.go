//go:build !darwin && !linux && !windows

package main

import "context"

// A portable pid record cannot prove current process ownership. Keep a held
// state excluded from auto-start, but never authorize signals from that record.
func processOwnsState(context.Context, int, string) bool { return false }
func legacyStateHolder(context.Context, string) int      { return 0 }
