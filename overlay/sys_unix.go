//go:build unix

package overlay

import (
	"bytes"
	"encoding/binary"
	"os"
	"syscall"
)

// encodeSys serializes the native stat record of host metadata for a stored
// layer's manifest; nil when info carries none.
func encodeSys(info os.FileInfo) ([]byte, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.NativeEndian, st); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeSys restores a record encodeSys wrote, as the *syscall.Stat_t host
// metadata carries; nil data restores no record.
func decodeSys(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	st := new(syscall.Stat_t)
	if err := binary.Read(bytes.NewReader(data), binary.NativeEndian, st); err != nil {
		return nil, err
	}
	return st, nil
}
