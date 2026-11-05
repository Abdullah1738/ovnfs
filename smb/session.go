package smb

import (
	"encoding/binary"
	"sync"
	"sync/atomic"

	"github.com/stencil-hq/ovnfs/smb/auth"
)

// sessionIDs supplies nonzero process-wide session identifiers.
var sessionIDs atomic.Uint64

// sessionExchange serializes authentication and distinguishes pending sessions from authenticated ones.
type sessionExchange struct {
	mu            sync.Mutex
	exchange      auth.Exchange
	active        bool
	authenticated bool
}

// init registers session lifecycle handlers.
func init() {
	handlers[cmdSessionSetup] = handleSessionSetup
	handlers[cmdLogoff] = handleLogoff
}

// sessionAuthenticated prevents a pending NTLM exchange from granting access to trees or files.
func sessionAuthenticated(s *session) bool {
	if s == nil {
		return false
	}
	state, ok := s.auth.(*sessionExchange)
	if !ok {
		return true
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.authenticated
}

// sessionSetupReply wraps a security token in the SMB2 SESSION_SETUP response.
func sessionSetupReply(s *session, token []byte, final bool) []byte {
	body := make([]byte, 8, 8+len(token))
	binary.LittleEndian.PutUint16(body[0:2], 9)
	if final {
		var flags uint16
		if s.guest {
			flags |= 1
		}
		if s.anonymous {
			flags |= 2
		}
		binary.LittleEndian.PutUint16(body[2:4], flags)
	}
	if len(token) != 0 {
		binary.LittleEndian.PutUint16(body[4:6], 72)
		binary.LittleEndian.PutUint16(body[6:8], uint16(len(token)))
	}
	return append(body, token...)
}

// handleSessionSetup runs NTLM authentication and derives the dialect's signing key.
func handleSessionSetup(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 24 || binary.LittleEndian.Uint16(b[:2]) != 25 || r.c.dialect == 0 {
		return nil, statusInvalidParameter
	}
	if b[2]&1 != 0 { // SMB2_SESSION_FLAG_BINDING requires unsupported multichannel.
		return nil, statusRequestNotAccepted
	}
	if b[2]&^byte(1) != 0 {
		return nil, statusInvalidParameter
	}
	offset := int(binary.LittleEndian.Uint16(b[12:14]))
	length := int(binary.LittleEndian.Uint16(b[14:16]))
	var token []byte
	if length != 0 {
		if offset < 88 || offset-64 > len(b) || length > len(b)-(offset-64) {
			return nil, statusInvalidParameter
		}
		token = b[offset-64 : offset-64+length]
	}
	// PreviousSessionId is intentionally ignored: reconnect creates a fresh session.
	var s *session
	fresh := r.hdr.SessionID == 0
	if fresh {
		id := sessionIDs.Add(1)
		if id == 0 {
			id = sessionIDs.Add(1)
		}
		s = &session{id: id, auth: &sessionExchange{}}
		r.c.addSession(s)
		r.hdr.SessionID = id
		r.sess = s
	} else {
		s = r.c.session(r.hdr.SessionID)
		if s == nil {
			return nil, statusUserSessionDeleted
		}
		r.sess = s
	}
	state, ok := s.auth.(*sessionExchange)
	if !ok {
		return nil, statusUserSessionDeleted
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.active {
		state.exchange = auth.Exchange{}
		state.active = true
		if r.c.dialect == 0x0311 {
			s.preauth = r.c.preauth
		}
	}
	if r.c.dialect == 0x0311 {
		s.preauth = preauthDigest(s.preauth, r.raw)
	}
	out, done, err := state.exchange.Step(&auth.Server{Users: r.c.srv.Users, Domain: "OVNFS"}, token)
	if err != nil {
		if r.c.srv.Logf != nil {
			r.c.srv.Logf("smb %s session=%d authentication: %v", r.c.nc.RemoteAddr(), s.id, err)
		}
		state.active = false
		if !state.authenticated {
			r.c.removeSession(s.id)
		}
		return nil, statusLogonFailure
	}
	if len(out) > 65535 {
		state.active = false
		if !state.authenticated {
			r.c.removeSession(s.id)
		}
		return nil, statusInternalError
	}
	if !done {
		if r.c.dialect == 0x0311 {
			r.onSent = func(msg []byte) {
				state.mu.Lock()
				s.preauth = preauthDigest(s.preauth, msg)
				state.mu.Unlock()
			}
		}
		return sessionSetupReply(s, out, false), statusMoreProcessingRequired
	}
	result := state.exchange.Result()
	s.user, s.guest, s.anonymous = result.User, result.Guest, result.Anonymous
	s.signRequired = len(r.c.srv.Users) != 0 && !s.guest
	s.signingKey = nil
	if len(result.SessionKey) != 0 && !s.guest && !s.anonymous {
		switch r.c.dialect {
		case 0x0202, 0x0210:
			s.signingKey = append([]byte(nil), result.SessionKey...)
		case 0x0300, 0x0302:
			s.signingKey = auth.KDF(result.SessionKey, []byte("SMB2AESCMAC\x00"), []byte("SmbSign\x00"), 128)
		case 0x0311:
			s.signingKey = auth.KDF(result.SessionKey, []byte("SMBSigningKey\x00"), s.preauth[:], 128)
		}
	}
	state.active = false
	state.authenticated = true
	if r.c.srv.Logf != nil {
		r.c.srv.Logf("smb %s session=%d user=%q guest=%t anonymous=%t signing_required=%t", r.c.nc.RemoteAddr(), s.id, s.user, s.guest, s.anonymous, s.signRequired)
	}
	return sessionSetupReply(s, out, true), statusSuccess
}

// handleLogoff closes every tree and open associated with an authenticated session.
func handleLogoff(r *request) ([]byte, uint32) {
	if len(r.body) < 4 || binary.LittleEndian.Uint16(r.body[:2]) != 4 {
		return nil, statusInvalidParameter
	}
	if r.sess == nil {
		return nil, statusUserSessionDeleted
	}
	r.c.removeSession(r.sess.id)
	return []byte{4, 0, 0, 0}, statusSuccess
}
