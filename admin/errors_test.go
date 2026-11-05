//go:build darwin || linux || windows

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// codedFailure exercises wrapped application error classification.
type codedFailure struct{}

// Error returns the failure description.
func (codedFailure) Error() string { return "checkout missing" }

// ErrorCode identifies the wire error category.
func (codedFailure) ErrorCode() string { return "not_found" }

// TestErrorMapping preserves application codes and maps ordinary failures to io.
func TestErrorMapping(t *testing.T) {
	path := testServer(t, testHandler(func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		if op == "slow" {
			return nil, fmt.Errorf("lookup: %w", codedFailure{})
		}
		return nil, errors.New("disk failure")
	}), nil)
	c, ctx := testClient(t, path)
	for _, test := range []struct{ op, code string }{{"slow", "not_found"}, {"echo", "io"}} {
		_, err := c.Call(ctx, test.op, struct{}{})
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.Code != test.code {
			t.Fatalf("%s: %v", test.op, err)
		}
	}
}

// TestCallCancellation preserves a pending sibling and its event subscription.
func TestCallCancellation(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	slowStopped := make(chan struct{})
	events := make(chan Event, 1)
	path := testServer(t, testHandler(func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		started <- op
		if op == "slow" {
			<-ctx.Done()
			close(slowStopped)
			return nil, ctx.Err()
		}
		select {
		case <-release:
			return "sibling completed", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), func(context.Context) <-chan Event { return events })
	c, ctx := testClient(t, path)
	stream, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	result, sibling := make(chan error, 1), make(chan response, 1)
	go func() { _, err := c.Call(canceled, "slow", struct{}{}); result <- err }()
	go func() {
		value, err := c.Call(ctx, "echo", struct{}{})
		sibling <- response{value: value, err: err}
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-slowStopped:
	case <-ctx.Done():
		t.Fatal("canceled handler did not stop")
	}
	select {
	case reply := <-sibling:
		t.Fatalf("pending sibling ended during cancellation: %s, %v", reply.value, reply.err)
	default:
	}
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending calls after cancellation: %d", pending)
	}
	events <- Event{Name: "alive", Payload: true}
	select {
	case event, ok := <-stream:
		if !ok || string(event) != `{"alive":true}` {
			t.Fatalf("subscription after cancellation: %s, open=%v", event, ok)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(release)
	select {
	case reply := <-sibling:
		if reply.err != nil || string(reply.value) != `"sibling completed"` {
			t.Fatalf("sibling reply: %s, %v", reply.value, reply.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
