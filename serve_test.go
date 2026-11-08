package main

import (
	"net"
	"runtime"
	"testing"
)

// TestFileListenerCutWaitsOutMountCalls guards restart under load: a call into
// the mount in flight when a keep-mount stop cuts the listener must still be
// served, and none may start after the cut, or the old daemon hangs on a mount
// whose next server waits for it to exit.
func TestFileListenerCutWaitsOutMountCalls(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &fileListener{Listener: ln, conns: map[*fileConn]struct{}{}}
	entered, release, cut := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go l.serving(func() {
		close(entered)
		<-release
	})
	<-entered
	go func() {
		l.cut()
		close(cut)
	}()
	// TryRLock fails once cut waits for the call to return.
	for l.calls.TryRLock() {
		l.calls.RUnlock()
		select {
		case <-cut:
			t.Fatal("cut returned while a call into the mount was in flight")
		default:
			runtime.Gosched()
		}
	}
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := l.Accept(); err != nil {
		t.Fatalf("listener stopped serving before the call in flight returned: %v", err)
	}
	close(release)
	<-cut
	l.serving(func() { t.Error("call into the mount ran after the cut") })
}
