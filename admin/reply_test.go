//go:build darwin || linux || windows

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOversizedReplyReturnsError(t *testing.T) {
	path := testServer(t, testHandler(func(_ context.Context, op string, _ json.RawMessage) (any, error) {
		if op == "slow" {
			return strings.Repeat("x", maxFrame), nil
		}
		return "usable", nil
	}), nil)
	client, ctx := testClient(t, path)
	_, err := client.Call(ctx, "slow", struct{}{})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "too_large" {
		t.Fatalf("oversized reply: %v", err)
	}
	got, err := client.Call(ctx, "echo", struct{}{})
	if err != nil || string(got) != `"usable"` {
		t.Fatalf("connection after oversized reply: %s, %v", got, err)
	}
}
