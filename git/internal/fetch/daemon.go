package fetch

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// defaultGitPort is the git:// daemon's port.
const defaultGitPort = "9418"

// tcpConn is a git:// connection; cancelling the context closes it.
type tcpConn struct {
	net.Conn
	stop func() bool
}

// dialGit connects to a git daemon and asks for upload-pack, requesting
// protocol v2 as an extra parameter (gitprotocol-pack "Git Transport").
func dialGit(ctx context.Context, ep *endpoint) (*tcpConn, error) {
	port := ep.port
	if port == "" {
		port = defaultGitPort
	}
	// NULs separate the request's parameters, so neither field may hold one.
	if strings.ContainsRune(ep.path, 0) || strings.ContainsRune(ep.hostParam, 0) {
		return nil, fmt.Errorf("fetch: NUL byte in git:// host or path")
	}
	var w pktWriter
	w.data([]byte("git-upload-pack " + ep.path + "\x00host=" + ep.hostParam + "\x00\x00version=2\x00"))
	if w.err != nil {
		return nil, w.err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ep.host, port))
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	tc := &tcpConn{Conn: c, stop: context.AfterFunc(ctx, func() { c.Close() })}
	if _, err := c.Write(w.buf); err != nil {
		tc.finish(nil)
		return nil, fmt.Errorf("fetch: %w", err)
	}
	return tc, nil
}

func (tc *tcpConn) finish(err error) error {
	tc.stop()
	tc.Conn.Close()
	return err
}
