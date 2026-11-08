package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
)

// TestCaptureProfile checks every kind returns base64 gzip pprof bytes, that a
// sampling kind honours the requested window, and that a snapshot reports none.
func TestCaptureProfile(t *testing.T) {
	for _, kind := range profileKinds {
		t.Run(kind, func(t *testing.T) {
			reply, err := captureProfile(t.Context(), api.ProfileArgs{Kind: kind, Seconds: 0.02})
			if err != nil {
				t.Fatal(err)
			}
			data, err := base64.StdEncoding.DecodeString(reply.Data)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Kind != kind || reply.Bytes != len(data) || len(data) == 0 {
				t.Fatalf("reply %#v, %d decoded bytes", reply, len(data))
			}
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("profile is not gzip: %v", err)
			}
			zr.Close()
			sampled := kind == "cpu" || kind == "block" || kind == "mutex"
			if sampled != (reply.Seconds > 0) {
				t.Fatalf("seconds %v for kind %s", reply.Seconds, kind)
			}
		})
	}
}

// TestProfileWindow checks the sampling window: defaulted when unset, negative
// or NaN, capped when huge, and exact in between.
func TestProfileWindow(t *testing.T) {
	for _, tt := range []struct {
		seconds float64
		want    time.Duration
	}{
		{seconds: 0, want: profileWindowDefault},
		{seconds: -5, want: profileWindowDefault},
		{seconds: math.NaN(), want: profileWindowDefault},
		{seconds: 2, want: 2 * time.Second},
		{seconds: 1e9, want: profileWindowMax},
	} {
		if got := profileWindow(tt.seconds); got != tt.want {
			t.Errorf("profileWindow(%v) = %v; want %v", tt.seconds, got, tt.want)
		}
	}
}

// TestCaptureProfileUnknownKind rejects an unsupported kind as invalid.
func TestCaptureProfileUnknownKind(t *testing.T) {
	_, err := captureProfile(t.Context(), api.ProfileArgs{Kind: "bogus"})
	var ce *checkout.Error
	if !errors.As(err, &ce) || ce.Code != "invalid" {
		t.Fatalf("err %v", err)
	}
}
