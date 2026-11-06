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

// ReparseTagMountPoint is IO_REPARSE_TAG_MOUNT_POINT, a junction.
const ReparseTagMountPoint uint32 = 0xa0000003

// ErrNotSymlink reports a reparse buffer that is neither a symlink nor a junction.
var ErrNotSymlink = errors.New("ntinfo: reparse tag is not IO_REPARSE_TAG_SYMLINK or IO_REPARSE_TAG_MOUNT_POINT")

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
// into a POSIX-style link target, for FSCTL_SET_REPARSE_POINT. A junction's
// IO_REPARSE_TAG_MOUNT_POINT buffer decodes to its absolute target, so the
// junction is stored as a symlink.
func ParseSymlinkReparse(b []byte) (string, error) {
	if len(b) < 8 || int(binary.LittleEndian.Uint16(b[4:])) != len(b)-8 {
		return "", fmt.Errorf("ntinfo: malformed reparse header: %w", fs.ErrInvalid)
	}
	// Both layouts start with the substitute name's offset and length; a
	// junction's path buffer follows at 16, a symlink's after its flags at 20.
	tag := binary.LittleEndian.Uint32(b)
	names := 20
	switch tag {
	case ReparseTagSymlink:
	case ReparseTagMountPoint:
		names = 16
	default:
		return "", ErrNotSymlink
	}
	if len(b) < names {
		return "", fmt.Errorf("ntinfo: short reparse buffer: %w", fs.ErrInvalid)
	}
	off, n := int(binary.LittleEndian.Uint16(b[8:])), int(binary.LittleEndian.Uint16(b[10:]))
	if off&1 != 0 || n&1 != 0 || off > len(b)-names || n > len(b)-names-off || tag == ReparseTagSymlink && binary.LittleEndian.Uint32(b[16:])&^uint32(1) != 0 {
		return "", fmt.Errorf("ntinfo: malformed substitute name: %w", fs.ErrInvalid)
	}
	u := make([]uint16, n/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[names+off+2*i:])
	}
	target := strings.ReplaceAll(string(utf16.Decode(u)), "\\", "/")
	if strings.HasPrefix(target, "/??/UNC/") {
		target = "//" + target[8:]
	} else {
		target = strings.TrimPrefix(target, "/??/")
	}
	if target == "" || strings.ContainsRune(target, 0) || tag == ReparseTagMountPoint && !IsAbs(target) {
		return "", fmt.Errorf("ntinfo: invalid symlink target: %w", fs.ErrInvalid)
	}
	return target, nil
}
