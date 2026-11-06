package node

import (
	"errors"
	"fmt"
	"strings"

	"github.com/quic-go/quic-go"
	"github.com/stencil-hq/ovnfs/mesh"
)

// ErrNoWays reports a node the peer book knows no way to.
var ErrNoWays = errors.New("mesh: no way to the node is known")

// ErrClosed reports a node or listener that is shut down.
var ErrClosed = errors.New("mesh: node is shut down")

// ErrNotDeclared reports a Listen on a protocol missing from Config.ALPNs.
var ErrNotDeclared = errors.New("mesh: protocol not declared in Config.ALPNs")

// ErrListening reports a Listen on a protocol another listener serves.
var ErrListening = errors.New("mesh: protocol already has a listener")

// RefusedError is the peer refusing the connection (close code 1): not
// admitted, rate-limited, or too slow.
type RefusedError struct{ Reason string }

// Error names the peer's reason.
func (e *RefusedError) Error() string { return "mesh: refused by the peer: " + e.Reason }

// NotServingError is the peer declaring the protocol without a live
// listener for it (close code 5).
type NotServingError struct{ ALPN string }

// Error names the protocol.
func (e *NotServingError) Error() string {
	return fmt.Sprintf("mesh: the peer does not serve %q now", e.ALPN)
}

// BusyError is the peer's listener backlog being full (close code 6); retry
// with backoff.
type BusyError struct{ ALPN string }

// Error names the protocol.
func (e *BusyError) Error() string { return fmt.Sprintf("mesh: the peer is busy on %q", e.ALPN) }

// NoProtocolError is the peer not speaking the protocol at all (TLS found
// no common ALPN).
type NoProtocolError struct{ ALPN string }

// Error names the protocol.
func (e *NoProtocolError) Error() string {
	return fmt.Sprintf("mesh: the peer does not speak %q", e.ALPN)
}

// WrongKeyError is a way answered by a key other than the expected node's.
type WrongKeyError struct {
	Expected mesh.NodeID
	Seen     mesh.NodeID
}

// Error names both keys.
func (e *WrongKeyError) Error() string {
	return fmt.Sprintf("mesh: the key there is %s, not node %s's", e.Seen.Fingerprint(), e.Expected)
}

// WayFailure is why one way failed in a race.
type WayFailure struct {
	Way mesh.Way
	Err error
}

// UnreachableError is every way failing, in the order the ways started.
type UnreachableError struct{ Tried []WayFailure }

// Error lists each way and its failure.
func (e *UnreachableError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "mesh: unreachable on %d ways", len(e.Tried))
	for _, f := range e.Tried {
		fmt.Fprintf(&b, "; %s: %v", f.Way, f.Err)
	}
	return b.String()
}

// Unwrap is each way's error, so errors.As finds a WrongKeyError.
func (e *UnreachableError) Unwrap() []error {
	errs := make([]error, len(e.Tried))
	for i, f := range e.Tried {
		errs[i] = f.Err
	}
	return errs
}

// noApplicationProtocol is TLS alert 120 as a QUIC crypto error.
const noApplicationProtocol = 0x100 + 120

// answer is the peer's own answer in how err ended a dial on alpn (refused,
// not serving, busy, not speaking it), or nil when the way merely failed.
func answer(err error, alpn string) error {
	if ae, ok := errors.AsType[*quic.ApplicationError](err); ok && ae.Remote {
		switch ae.ErrorCode {
		case mesh.Denied:
			return &RefusedError{Reason: ae.ErrorMessage}
		case mesh.NotServing:
			return &NotServingError{ALPN: alpn}
		case mesh.Busy:
			return &BusyError{ALPN: alpn}
		}
		return nil
	}
	if te, ok := errors.AsType[*quic.TransportError](err); ok && te.Remote && te.ErrorCode == noApplicationProtocol {
		return &NoProtocolError{ALPN: alpn}
	}
	return nil
}
