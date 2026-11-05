package smb

import (
	"encoding/binary"
	"errors"
)

// SMB2 command numbers are assigned consecutively by MS-SMB2 section 2.2.
const (
	cmdNegotiate uint16 = iota
	cmdSessionSetup
	cmdLogoff
	cmdTreeConnect
	cmdTreeDisconnect
	cmdCreate
	cmdClose
	cmdFlush
	cmdRead
	cmdWrite
	cmdLock
	cmdIoctl
	cmdCancel
	cmdEcho
	cmdQueryDirectory
	cmdChangeNotify
	cmdQueryInfo
	cmdSetInfo
	cmdOplockBreak
)

// Header flags distinguish responses, async messages, compounds, and signatures.
const (
	flagResponse     uint32 = 1
	flagAsync        uint32 = 2
	flagRelated      uint32 = 4
	flagSigned       uint32 = 8
	flagPriorityMask uint32 = 0x70
	flagDFS          uint32 = 0x10000000
	flagReplay       uint32 = 0x20000000
)

// header is the common 64-byte SMB2 synchronous or asynchronous header.
type header struct {
	CreditCharge uint16
	Status       uint32
	Command      uint16
	Credits      uint16
	Flags        uint32
	NextCommand  uint32
	MessageID    uint64
	AsyncID      uint64
	TreeID       uint32
	SessionID    uint64
	Signature    [16]byte
}

// decodeHeader validates the SMB2 header and decodes its union by Flags.
func decodeHeader(b []byte) (header, error) {
	var h header
	if len(b) < 64 || string(b[:4]) != "\xfeSMB" || binary.LittleEndian.Uint16(b[4:6]) != 64 {
		return h, errors.New("smb: invalid SMB2 header")
	}
	h.CreditCharge = binary.LittleEndian.Uint16(b[6:8])
	h.Status = binary.LittleEndian.Uint32(b[8:12])
	h.Command = binary.LittleEndian.Uint16(b[12:14])
	h.Credits = binary.LittleEndian.Uint16(b[14:16])
	h.Flags = binary.LittleEndian.Uint32(b[16:20])
	h.NextCommand = binary.LittleEndian.Uint32(b[20:24])
	h.MessageID = binary.LittleEndian.Uint64(b[24:32])
	if h.Flags&flagAsync != 0 {
		h.AsyncID = binary.LittleEndian.Uint64(b[32:40])
	} else {
		h.TreeID = binary.LittleEndian.Uint32(b[36:40])
	}
	h.SessionID = binary.LittleEndian.Uint64(b[40:48])
	copy(h.Signature[:], b[48:64])
	return h, nil
}

// appendTo appends the exact wire representation, leaving the reserved fields zero.
func (h *header) appendTo(dst []byte) []byte {
	start := len(dst)
	dst = append(dst, make([]byte, 64)...)
	b := dst[start:]
	copy(b, "\xfeSMB")
	binary.LittleEndian.PutUint16(b[4:6], 64)
	binary.LittleEndian.PutUint16(b[6:8], h.CreditCharge)
	binary.LittleEndian.PutUint32(b[8:12], h.Status)
	binary.LittleEndian.PutUint16(b[12:14], h.Command)
	binary.LittleEndian.PutUint16(b[14:16], h.Credits)
	binary.LittleEndian.PutUint32(b[16:20], h.Flags)
	binary.LittleEndian.PutUint32(b[20:24], h.NextCommand)
	binary.LittleEndian.PutUint64(b[24:32], h.MessageID)
	if h.Flags&flagAsync != 0 {
		binary.LittleEndian.PutUint64(b[32:40], h.AsyncID)
	} else {
		binary.LittleEndian.PutUint32(b[36:40], h.TreeID)
	}
	binary.LittleEndian.PutUint64(b[40:48], h.SessionID)
	copy(b[48:64], h.Signature[:])
	return dst
}
