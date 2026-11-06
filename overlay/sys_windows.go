//go:build windows

package overlay

import (
	"bytes"
	"encoding/binary"
	"os"
	"syscall"
)

// encodeSys serializes the native attribute record of host metadata for a
// stored layer's manifest; nil when info carries none.
func encodeSys(info os.FileInfo) ([]byte, error) {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || data == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.NativeEndian, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeSys restores a record encodeSys wrote, as the
// *syscall.Win32FileAttributeData host metadata carries; nil data restores no record.
func decodeSys(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	attrs := new(syscall.Win32FileAttributeData)
	if err := binary.Read(bytes.NewReader(data), binary.NativeEndian, attrs); err != nil {
		return nil, err
	}
	return attrs, nil
}
