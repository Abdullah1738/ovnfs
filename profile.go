package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
)

// profileKinds names the runtime profiles the daemon captures; see api.ProfileArgs.Kind.
var profileKinds = []string{"cpu", "heap", "allocs", "goroutine", "block", "mutex", "threadcreate"}

// profileWindowDefault bounds a sampling profile (cpu, block, mutex) when the
// request names no duration.
const profileWindowDefault = 30 * time.Second

// profileWindowMax caps a sampling window so a request cannot tie up a caller
// for longer, and keeps the duration within a time.Duration.
const profileWindowMax = time.Hour

// maxProfileData caps a profile so one base64 reply stays within an admin frame
// (admin.maxFrame, 16 MiB, which base64 inflates by a third).
const maxProfileData = 11 << 20

// captureProfile writes a pprof profile of the requested kind, base64 because it
// shares the JSON admin protocol with the checkout operations. cpu, block and
// mutex accumulate samples over a window, so the sampler is enabled, awaited and
// restored; the rest are instantaneous snapshots.
func captureProfile(ctx context.Context, a api.ProfileArgs) (api.ProfileReply, error) {
	kind := a.Kind
	if kind == "" {
		kind = "cpu"
	}
	if !slices.Contains(profileKinds, kind) {
		return api.ProfileReply{}, &checkout.Error{Code: "invalid", Message: fmt.Sprintf("unknown profile %q; have %s", kind, strings.Join(profileKinds, ", "))}
	}
	window := profileWindow(a.Seconds)
	sampled := kind == "cpu" || kind == "block" || kind == "mutex"
	var buf bytes.Buffer
	stop := func() {}
	switch kind {
	case "cpu":
		if err := pprof.StartCPUProfile(&buf); err != nil {
			return api.ProfileReply{}, &checkout.Error{Code: "busy", Message: "cpu profile: " + err.Error()}
		}
		stop = pprof.StopCPUProfile
	case "block":
		// The daemon never samples the block profile itself; rate 0 is the
		// default, so restoring it leaves the process as it was found.
		runtime.SetBlockProfileRate(1)
		stop = func() { runtime.SetBlockProfileRate(0) }
	case "mutex":
		previous := runtime.SetMutexProfileFraction(1)
		stop = func() { runtime.SetMutexProfileFraction(previous) }
	}
	if sampled {
		if err := sleepCtx(ctx, window); err != nil {
			stop()
			return api.ProfileReply{}, err
		}
	}
	var err error
	if kind != "cpu" {
		// StopCPUProfile flushes cpu into buf; the others write on demand.
		err = pprof.Lookup(kind).WriteTo(&buf, 0)
	}
	stop()
	if err != nil {
		return api.ProfileReply{}, fmt.Errorf("write %s profile: %w", kind, err)
	}
	if buf.Len() > maxProfileData {
		return api.ProfileReply{}, &checkout.Error{Code: "too_large", Message: fmt.Sprintf("%s profile is %d bytes, over the %d-byte limit; use a shorter --seconds", kind, buf.Len(), maxProfileData)}
	}
	seconds := 0.0
	if sampled {
		seconds = window.Seconds()
	}
	return api.ProfileReply{Kind: kind, Seconds: seconds, Bytes: buf.Len(), Data: base64.StdEncoding.EncodeToString(buf.Bytes())}, nil
}

// profileWindow picks a sampling window: the request's seconds when positive,
// else the default, never past profileWindowMax. Comparing before the Duration
// conversion keeps a huge or NaN request from overflowing into a negative wait.
func profileWindow(seconds float64) time.Duration {
	if seconds > profileWindowMax.Seconds() {
		return profileWindowMax
	}
	if seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return profileWindowDefault
}

// sleepCtx waits for d, returning ctx.Err() when the caller is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runProfile captures a runtime profile from the daemon and writes its pprof
// bytes to FILE (-o) or stdout, so `ovnfs profile cpu > cpu.pb.gz` and
// `go tool pprof cpu.pb.gz` both work. With --json the reply is printed raw.
func runProfile(c *cli, args []string) error {
	fs := c.flags("profile")
	seconds := fs.Float64("seconds", 0, "sampling window in seconds (default 30, cpu/block/mutex only)")
	out := fs.String("o", "", "write the profile to FILE instead of stdout")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	a := api.ProfileArgs{Seconds: *seconds}
	if len(pos) == 1 {
		a.Kind = pos[0]
	}
	reply, err := call[api.ProfileReply](c, "profile", a)
	if err != nil {
		return err
	}
	if c.json {
		return nil // call already printed the raw reply
	}
	data, err := base64.StdEncoding.DecodeString(reply.Data)
	if err != nil {
		return fmt.Errorf("profile: decoding reply: %w", err)
	}
	if *out == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s profile (%s) to %s\n", reply.Kind, humanBytes(int64(reply.Bytes)), *out)
	return nil
}
