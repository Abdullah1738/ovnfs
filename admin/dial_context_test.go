package admin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDialContextCancelsSilentHandshake(t *testing.T) {
	dir, err := os.MkdirTemp("", "dial-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "admin.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c, err := DialContext(ctx, path, "test")
		if c != nil {
			c.Close()
		}
		done <- err
	}()
	c := <-accepted
	if c == nil {
		t.Fatal("accept failed")
	}
	defer c.Close()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("silent peer accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled handshake hung")
	}
}
