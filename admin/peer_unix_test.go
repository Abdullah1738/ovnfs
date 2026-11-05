//go:build darwin || linux

package admin

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// TestPeerUID verifies uid comparison and that the socket is private to its owner.
func TestPeerUID(t *testing.T) {
	if err := checkUID(42, 42); err != nil {
		t.Fatal(err)
	}
	if err := checkUID(42, 43); err == nil {
		t.Fatal("accepted different uid")
	}
	path := testServer(t, testHandler(func(context.Context, string, json.RawMessage) (any, error) { return struct{}{}, nil }), nil)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions %o", info.Mode().Perm())
	}
}
