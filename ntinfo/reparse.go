package ntinfo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf16"
)

// ReparseTagSymlink is IO_REPARSE_TAG_SYMLINK.
const ReparseTagSymlink uint32 = 0xa000000c

// ErrNotSymlink reports a reparse buffer with a tag other than ReparseTagSymlink.
var ErrNotSymlink = errors.New("ntinfo: reparse tag is not IO_REPARSE_TAG_SYMLINK")

// IsAbs reports whether a stored, slash-separated link target is absolute:
// rooted (/x), UNC (//srv/share) or a drive path (C:/x). Hosts store targets
// in their own form, so a server on any OS must not resolve these as relative.
func IsAbs(target string) bool {
	return strings.HasPrefix(target, "/") || isDrivePath(target)
}

// isDrivePath reports a target such as C:/x.
func isDrivePath(t string) bool {
	return len(t) >= 3 && t[1] == ':' && t[2] == '/' && ('A' <= t[0] && t[0] <= 'Z' || 'a' <= t[0] && t[0] <= 'z')
}

// SymlinkReparse encodes target as an IO_REPARSE_TAG_SYMLINK REPARSE_DATA_BUFFER
// (header included), for FSCTL_GET_REPARSE_POINT. Drive and UNC targets get
// the NT substitute name Windows resolves (\??\C:\x, \??\UNC\srv\share) and
// their usual form as print name; other targets use one name for both.
func SymlinkReparse(target string) ([]byte, error) {
	print := strings.ReplaceAll(target, "/", "\\")
	substitute := print
	switch {
	case strings.HasPrefix(target, "//?/") || strings.HasPrefix(target, "//./"):
		// Win32 device paths (\\?\C:\x, \\?\Volume{…}\) name the NT \??\ directory.
		substitute = `\??\` + print[4:]
	case strings.HasPrefix(target, "//"):
		substitute = `\??\UNC\` + print[2:]
	case isDrivePath(target):
		substitute = `\??\` + print
	}
	sub, prn := utf16.Encode([]rune(substitute)), utf16.Encode([]rune(print))
	n := 2 * (len(sub) + len(prn))
	if 12+n > 65535 {
		return nil, fmt.Errorf("ntinfo: symlink target too long: %w", fs.ErrInvalid)
	}
	b := make([]byte, 20, 20+n)
	binary.LittleEndian.PutUint32(b, ReparseTagSymlink)
	binary.LittleEndian.PutUint16(b[4:], uint16(12+n))
	binary.LittleEndian.PutUint16(b[10:], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(b[12:], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(b[14:], uint16(2*len(prn)))
	if !IsAbs(target) {
		binary.LittleEndian.PutUint32(b[16:], 1) // SYMLINK_FLAG_RELATIVE
	}
	for _, name := range [2][]uint16{sub, prn} {
		for _, u := range name {
			b = binary.LittleEndian.AppendUint16(b, u)
		}
	}
	return b, nil
}

// ParseSymlinkReparse decodes an IO_REPARSE_TAG_SYMLINK buffer (header included)
// into a POSIX-style link target, for FSCTL_SET_REPARSE_POINT.
func ParseSymlinkReparse(b []byte) (string, error) {
	if len(b) < 8 || int(binary.LittleEndian.Uint16(b[4:])) != len(b)-8 {
		return "", fmt.Errorf("ntinfo: malformed reparse header: %w", fs.ErrInvalid)
	}
	if binary.LittleEndian.Uint32(b) != ReparseTagSymlink {
		return "", ErrNotSymlink
	}
	if len(b) < 20 {
		return "", fmt.Errorf("ntinfo: short symlink reparse buffer: %w", fs.ErrInvalid)
	}
	off, n := int(binary.LittleEndian.Uint16(b[8:])), int(binary.LittleEndian.Uint16(b[10:]))
	flags := binary.LittleEndian.Uint32(b[16:])
	if off&1 != 0 || n&1 != 0 || off > len(b)-20 || n > len(b)-20-off || flags&^uint32(1) != 0 {
		return "", fmt.Errorf("ntinfo: malformed symlink substitute name: %w", fs.ErrInvalid)
	}
	u := make([]uint16, n/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[20+off+2*i:])
	}
	target := strings.ReplaceAll(string(utf16.Decode(u)), "\\", "/")
	if strings.HasPrefix(target, "/??/UNC/") {
		target = "//" + target[8:]
	} else {
		target = strings.TrimPrefix(target, "/??/")
	}
	if target == "" || strings.ContainsRune(target, 0) {
		return "", fmt.Errorf("ntinfo: invalid symlink target: %w", fs.ErrInvalid)
	}
	return target, nil
}
