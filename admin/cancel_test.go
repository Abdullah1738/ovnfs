//go:build darwin || linux || windows

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestCallCancellationStopsServerWork(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	path := testServer(t, testHandler(func(ctx context.Context, op string, _ json.RawMessage) (any, error) {
		if op == "slow" {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		}
		return "still usable", nil
	}), nil)
	client, ctx := testClient(t, path)
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.Call(request, "slow", struct{}{})
		result <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("canceled call left server work running")
	}
	got, err := client.Call(ctx, "echo", struct{}{})
	if err != nil || string(got) != `"still usable"` {
		t.Fatalf("connection after cancellation: %s, %v", got, err)
	}
}

func TestCallDeadlineStopsServerWork(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	path := testServer(t, testHandler(func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}), nil)
	client, ctx := testClient(t, path)
	request, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.Call(request, "slow", struct{}{})
		result <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller deadline: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("expired call left server work running")
	}
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("expired call left %d pending entries", pending)
	}
}

// rawTestClient performs negotiation while leaving request IDs under test control.
func rawTestClient(t *testing.T, path string) (net.Conn, context.Context) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	sendTestFrame(t, conn, map[string]any{"hello": struct{}{}})
	reply, err := readFrame(conn)
	if err != nil || reply["welcome"] == nil {
		t.Fatalf("welcome: %s, %v", reply["welcome"], err)
	}
	return conn, ctx
}

func awaitTestContext(t *testing.T, ctx context.Context, ch <-chan context.Context) context.Context {
	t.Helper()
	select {
	case request := <-ch:
		return request
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}

func sendTestFrame(t *testing.T, conn net.Conn, value any) {
	t.Helper()
	if err := writeFrame(conn, value); err != nil {
		t.Fatal(err)
	}
}

func readTestReply(t *testing.T, conn net.Conn, id uint64, code string) json.RawMessage {
	t.Helper()
	reply, err := readFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	var gotID uint64
	if err := json.Unmarshal(reply["id"], &gotID); err != nil || gotID != id {
		t.Fatalf("reply id %s, want %d: %v", reply["id"], id, err)
	}
	if code == "" {
		if reply["error"] != nil || reply["ok"] == nil {
			t.Fatalf("expected success: %v", reply)
		}
		return reply["ok"]
	}
	var remote RemoteError
	if err := json.Unmarshal(reply["error"], &remote); err != nil || remote.Code != code {
		t.Fatalf("reply error %s, want %s: %v", reply["error"], code, err)
	}
	return nil
}

// TestRawCancellationBeforeWork gates handler work until the read loop has
// processed cancellation, even if the handler goroutine has not started yet.
func TestRawCancellationBeforeWork(t *testing.T) {
	for _, outcome := range []string{"context error", "coded wrapper", "success"} {
		t.Run(outcome, func(t *testing.T) {
			started := make(chan context.Context, 1)
			completed := make(chan context.Context, 2)
			work, release := context.WithCancel(context.Background())
			defer release()
			path := testServer(t, testHandler(func(ctx context.Context, op string, _ json.RawMessage) (any, error) {
				if op != "slow" {
					completed <- ctx
					return "usable", nil
				}
				started <- ctx
				<-work.Done()
				switch outcome {
				case "context error":
					return nil, fmt.Errorf("search: %w", ctx.Err())
				case "coded wrapper":
					return nil, fmt.Errorf("checkout: %w", codedFailure{})
				default:
					return "partial results", nil
				}
			}), nil)
			conn, ctx := rawTestClient(t, path)
			sendTestFrame(t, conn, map[string]any{"id": 1, "slow": struct{}{}})
			sendTestFrame(t, conn, map[string]any{"cancel": 1})
			sendTestFrame(t, conn, map[string]any{"cancel": 999})
			sendTestFrame(t, conn, map[string]any{"cancel": 1})
			sendTestFrame(t, conn, map[string]any{"id": 2, "echo": struct{}{}})
			readTestReply(t, conn, 2, "")
			if requestCtx := awaitTestContext(t, ctx, started); requestCtx.Err() != context.Canceled {
				t.Fatalf("handler work started without cancellation: %v", requestCtx.Err())
			}
			if requestCtx := awaitTestContext(t, ctx, completed); requestCtx.Err() != context.Canceled {
				t.Fatalf("completed request context not released: %v", requestCtx.Err())
			}
			release()
			readTestReply(t, conn, 1, "canceled")
			// Completed and unknown cancellations are silent and cannot poison a
			// later request that reuses a released ID.
			sendTestFrame(t, conn, map[string]any{"cancel": 1})
			sendTestFrame(t, conn, map[string]any{"cancel": 2})
			sendTestFrame(t, conn, map[string]any{"id": 1, "echo": struct{}{}})
			readTestReply(t, conn, 1, "")
			if requestCtx := awaitTestContext(t, ctx, completed); requestCtx.Err() != context.Canceled {
				t.Fatalf("reused request context not released: %v", requestCtx.Err())
			}
		})
	}
}

func TestMalformedCancellationDoesNotInvokeHandler(t *testing.T) {
	started := make(chan context.Context, 1)
	unexpected := make(chan struct{}, 1)
	path := testServer(t, testHandler(func(ctx context.Context, op string, _ json.RawMessage) (any, error) {
		if op != "slow" {
			unexpected <- struct{}{}
			return struct{}{}, nil
		}
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}), nil)
	conn, ctx := rawTestClient(t, path)
	sendTestFrame(t, conn, map[string]any{"id": 1, "slow": struct{}{}})
	for _, frame := range []string{
		`{"cancel":null}`,
		`{"cancel":-1}`,
		`{"cancel":1.5}`,
		`{"cancel":"1"}`,
		`{"cancel":true}`,
		`{"cancel":{}}`,
		`{"cancel":[]}`,
		`{"cancel":18446744073709551616}`,
		`{"cancel":1,"id":1}`,
		`{"cancel":1,"echo":{}}`,
	} {
		sendTestFrame(t, conn, json.RawMessage(frame))
		readTestReply(t, conn, 0, "invalid")
	}
	select {
	case <-unexpected:
		t.Fatal("malformed cancellation invoked a handler")
	default:
	}
	requestCtx := awaitTestContext(t, ctx, started)
	if err := requestCtx.Err(); err != nil {
		t.Fatalf("malformed cancellation canceled an active request: %v", err)
	}
	sendTestFrame(t, conn, map[string]any{"cancel": 1})
	readTestReply(t, conn, 1, "canceled")
}

func TestDuplicateActiveRequestIDRejected(t *testing.T) {
	started := make(chan context.Context, 1)
	unexpected := make(chan struct{}, 1)
	path := testServer(t, testHandler(func(ctx context.Context, op string, _ json.RawMessage) (any, error) {
		if op != "slow" {
			unexpected <- struct{}{}
			return struct{}{}, nil
		}
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}), nil)
	conn, ctx := rawTestClient(t, path)
	sendTestFrame(t, conn, map[string]any{"id": 7, "slow": struct{}{}})
	sendTestFrame(t, conn, map[string]any{"id": 7, "echo": struct{}{}})
	readTestReply(t, conn, 7, "invalid")
	select {
	case <-unexpected:
		t.Fatal("duplicate request invoked a handler")
	default:
	}
	if requestCtx := awaitTestContext(t, ctx, started); requestCtx.Err() != nil {
		t.Fatalf("duplicate request canceled original: %v", requestCtx.Err())
	}
	sendTestFrame(t, conn, map[string]any{"cancel": 7})
	readTestReply(t, conn, 7, "canceled")
}

type cancelWriteFailureConn struct {
	net.Conn
	fail <-chan struct{}
	err  error
}

func (c *cancelWriteFailureConn) Write(p []byte) (int, error) {
	select {
	case <-c.fail:
		return 0, c.err
	default:
		return c.Conn.Write(p)
	}
}

func TestCancellationWriteFailurePreservesContextError(t *testing.T) {
	conn, server := net.Pipe()
	t.Cleanup(func() { server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fail := make(chan struct{})
	writeErr := errors.New("cancel write failed")
	client := &Client{
		conn:      &cancelWriteFailureConn{Conn: conn, fail: fail, err: writeErr},
		welcome:   Welcome{Ops: []string{"slow", "echo"}},
		pending:   make(map[uint64]chan response),
		done:      make(chan struct{}),
		eventWake: make(chan struct{}, 1),
	}
	t.Cleanup(func() { client.Close() })
	go client.readLoop()
	received := make(chan error, 2)
	go func() {
		for range 2 {
			_, err := readFrame(server)
			received <- err
			if err != nil {
				return
			}
		}
	}()
	request, stop := context.WithCancel(ctx)
	defer stop()
	canceled, sibling := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := client.Call(request, "slow", struct{}{})
		canceled <- err
	}()
	go func() {
		_, err := client.Call(ctx, "echo", struct{}{})
		sibling <- err
	}()
	for range 2 {
		select {
		case err := <-received:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(fail)
	stop()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller received write failure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-sibling:
		if !errors.Is(err, writeErr) {
			t.Fatalf("sibling did not receive transport failure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("write failure left %d pending entries", pending)
	}
}

func TestDisconnectCancelsEveryRequestAndSubscription(t *testing.T) {
	started := make(chan context.Context, 2)
	stopped := make(chan struct{}, 2)
	eventContext := make(chan context.Context, 1)
	path := testServer(t, testHandler(func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		started <- ctx
		<-ctx.Done()
		stopped <- struct{}{}
		return nil, ctx.Err()
	}), func(ctx context.Context) <-chan Event {
		eventContext <- ctx
		return nil
	})
	client, ctx := testClient(t, path)
	stream, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 2)
	for _, op := range []string{"slow", "echo"} {
		go func() {
			_, err := client.Call(ctx, op, struct{}{})
			result <- err
		}()
	}
	var requests [2]context.Context
	for i := range requests {
		select {
		case requests[i] = <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if requests[0] == requests[1] {
		t.Fatal("handlers share one request context")
	}
	client.Close()
	for range 2 {
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("disconnected call returned success")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-stopped:
		case <-ctx.Done():
			t.Fatal("disconnect left handler running")
		}
	}
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("subscription remains open")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if eventCtx := awaitTestContext(t, ctx, eventContext); eventCtx.Err() != context.Canceled {
		t.Fatalf("subscription context after disconnect: %v", eventCtx.Err())
	}
	for _, request := range requests {
		if request.Err() != context.Canceled {
			t.Fatalf("request context after disconnect: %v", request.Err())
		}
	}
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("disconnect left %d pending entries", pending)
	}
}
