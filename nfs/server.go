// Package nfs serves a Backend over NFSv3 (RFC 1813, with MOUNT v3) and
// NFSv4.0 (RFC 7530) on one TCP port.
//
// rpc.go frames ONC RPC records and dispatches calls to mount.go, v3*.go and
// v4*.go. File handles are the backend's stable file ids (8 bytes,
// big-endian), so they survive renames and server restarts. NFSv4 clients may
// get read delegations on files the backend calls cacheable; Server.Recall
// takes them back when a file changes outside this server.
//
// Example (not runnable):
//
//	srv := &nfs.Server{FS: backend, Concurrency: 64}
//	go srv.Serve(listener)
//	// a change made through another protocol:
//	srv.Recall(backend.FileID("co/src/main.go"))
package nfs

import (
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"time"
)

// Limits both NFS versions advertise.
const (
	// maxIO is the largest READ or WRITE (rtmax and wtmax, maxread and
	// maxwrite): the most Linux and macOS transfer per call.
	maxIO = 1 << 20
	// maxName is the longest file name (NAME_MAX).
	maxName = 255
	// maxLink is the most names a file may have (LINK_MAX on macOS).
	maxLink = 32767
)

// Server exports one Backend to every client that mounts any path.
// Credentials are not checked: the backend's own permissions apply.
type Server struct {
	FS Backend
	// Concurrency bounds the calls of one connection answered at once; zero
	// or one answers them one at a time, in order.
	Concurrency int
	Logf        func(format string, args ...any)

	once sync.Once
	// verf is this instance's write verifier: it changes when the server
	// restarts, telling clients to resend unstable writes.
	verf [8]byte
	dirs dirCache
	v3   v3State
	v4   v4State
}

// Serve accepts connections until the listener fails, and closes it.
func (s *Server) Serve(l net.Listener) error {
	defer l.Close()
	if s.FS == nil {
		return errors.New("nfs: filesystem is required")
	}
	s.start()
	var delay time.Duration
	for {
		nc, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Running out of descriptors (EMFILE) passes: back off rather
			// than stop serving, which would leave the mount dead.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.logf("nfs: accept: %v", err)
			time.Sleep(delay)
			continue
		}
		delay = 0
		c := &conn{srv: s, nc: nc}
		go c.serve()
	}
}

// start initializes per-instance state once, before the first connection.
func (s *Server) start() {
	s.once.Do(func() {
		_, _ = rand.Read(s.verf[:])
		s.v4.start(s)
	})
}

// Recall makes NFSv4 clients return their delegations of file id before it
// changes. Changes through this server recall conflicting delegations
// themselves; the embedder calls Recall for every other change to a file
// Backend.Cacheable admits. It does not block.
func (s *Server) Recall(id uint64) { s.v4.recall(id) }

// logf logs through Logf, if set.
func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
