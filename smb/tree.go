package smb

import (
	"encoding/binary"
	"strings"
)

// init registers share connection lifecycle handlers.
func init() {
	handlers[cmdTreeConnect] = handleTreeConnect
	handlers[cmdTreeDisconnect] = handleTreeDisconnect
}

// handleTreeConnect resolves a UNC share name without exposing host filesystem paths.
func handleTreeConnect(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 8 || binary.LittleEndian.Uint16(b[:2]) != 9 {
		return nil, statusInvalidParameter
	}
	if r.sess == nil || !sessionAuthenticated(r.sess) {
		return nil, statusUserSessionDeleted
	}
	flags := binary.LittleEndian.Uint16(b[2:4])
	// The 3.1.1 extension area does not change the encoded UNC path location.
	if r.c.dialect >= 0x0311 && flags&^uint16(7) != 0 {
		return nil, statusInvalidParameter
	}
	offset := int(binary.LittleEndian.Uint16(b[4:6]))
	length := int(binary.LittleEndian.Uint16(b[6:8]))
	if offset < 72 || length == 0 || length&1 != 0 || offset-64 > len(b) || length > len(b)-(offset-64) {
		return nil, statusInvalidParameter
	}
	unc := utf16Decode(b[offset-64 : offset-64+length])
	if !strings.HasPrefix(unc, `\\`) || strings.ContainsRune(unc, 0) {
		return nil, statusBadNetworkName
	}
	parts := strings.Split(unc[2:], `\`)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, statusBadNetworkName
	}
	share := r.c.srv.Share
	if share == "" {
		share = "ovnfs"
	}
	ipc := strings.EqualFold(parts[1], "IPC$")
	if !ipc && !strings.EqualFold(parts[1], share) {
		return nil, statusBadNetworkName
	}
	t := &tree{sess: r.sess, ipc: ipc}
	r.sess.addTree(t)
	r.hdr.TreeID = t.id
	r.tree = t
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[:2], 16)
	body[2] = 1 // SMB2_SHARE_TYPE_DISK.
	if ipc {
		body[2] = 2 // SMB2_SHARE_TYPE_PIPE.
	}
	// ShareFlags=0 is SMB2_SHAREFLAG_MANUAL_CACHING; no automatic offline caching.
	binary.LittleEndian.PutUint32(body[12:16], 0x001f01ff)
	return body, statusSuccess
}

// handleTreeDisconnect closes a share's opens before removing its tree identifier.
func handleTreeDisconnect(r *request) ([]byte, uint32) {
	if len(r.body) < 4 || binary.LittleEndian.Uint16(r.body[:2]) != 4 {
		return nil, statusInvalidParameter
	}
	if r.sess == nil {
		return nil, statusUserSessionDeleted
	}
	t := r.tree
	if t == nil {
		t = r.sess.tree(r.hdr.TreeID)
	}
	if t == nil {
		return nil, statusNetworkNameDeleted
	}
	r.sess.removeTree(t.id)
	return []byte{4, 0, 0, 0}, statusSuccess
}
