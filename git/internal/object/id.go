// Package object is git's object model: object names (ID) in either hash
// format, object kinds, file modes, tree entries, and the object database that
// stores objects loose and in packs.
//
// Everything here is written from git's documentation (gitformat-loose,
// gitformat-pack, gitrepository-layout, hash-function-transition), not from
// another implementation.
package object

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
)

// Format is a repository's object hash algorithm, named by extensions.objectFormat.
type Format uint8

const (
	// SHA1 is the default format: 20-byte object names, 40 hex digits.
	SHA1 Format = iota + 1
	// SHA256 is extensions.objectFormat=sha256: 32-byte names, 64 hex digits.
	SHA256
)

// ParseFormat maps an extensions.objectFormat value to its Format; "" (the
// extension unset) is SHA1.
func ParseFormat(name string) (Format, error) {
	switch name {
	case "", "sha1":
		return SHA1, nil
	case "sha256":
		return SHA256, nil
	}
	return 0, fmt.Errorf("unknown object format %q", name)
}

// String returns the extensions.objectFormat spelling.
func (f Format) String() string {
	switch f {
	case SHA1:
		return "sha1"
	case SHA256:
		return "sha256"
	}
	return "Format(" + strconv.Itoa(int(f)) + ")"
}

// Size is the length of a raw object name.
func (f Format) Size() int {
	if f == SHA256 {
		return sha256.Size
	}
	return sha1.Size
}

// HexSize is the length of a full hex object name.
func (f Format) HexSize() int { return 2 * f.Size() }

// New returns a hash computing object names, and the trailing checksums of
// index, pack and idx files, in this format.
func (f Format) New() hash.Hash {
	if f == SHA256 {
		return sha256.New()
	}
	return sha1.New()
}

// Zero is the null object name, which git uses for "no object" (a ref being
// created or deleted, an index entry removed).
func (f Format) Zero() ID { return ID{format: f} }

// ID is a full object name. The zero value is no object in no format; IDs of
// different formats never compare equal. ID is comparable and usable as a map key.
type ID struct {
	format Format
	raw    [sha256.Size]byte
}

// ParseID decodes a full hex object name, inferring its format from its length
// (40: SHA1, 64: SHA256). Upper-case digits are accepted.
func ParseID(s string) (ID, error) {
	switch len(s) {
	case SHA1.HexSize():
		return ParseIDFormat(SHA1, s)
	case SHA256.HexSize():
		return ParseIDFormat(SHA256, s)
	}
	return ID{}, fmt.Errorf("invalid object name %q", s)
}

// ParseIDFormat decodes a full hex object name of format f.
func ParseIDFormat(f Format, s string) (ID, error) {
	id := ID{format: f}
	if len(s) != f.HexSize() {
		return ID{}, fmt.Errorf("invalid %s object name %q", f, s)
	}
	if _, err := hex.Decode(id.raw[:f.Size()], []byte(s)); err != nil {
		return ID{}, fmt.Errorf("invalid %s object name %q", f, s)
	}
	return id, nil
}

// IDFromRaw wraps a raw object name of format f, as stored in trees, index
// entries and pack indexes.
func IDFromRaw(f Format, raw []byte) (ID, error) {
	id := ID{format: f}
	if len(raw) != f.Size() {
		return ID{}, fmt.Errorf("raw %s object name has %d bytes", f, len(raw))
	}
	copy(id.raw[:], raw)
	return id, nil
}

// Format returns the hash algorithm the name belongs to; 0 for the zero ID.
func (id ID) Format() Format { return id.format }

// Raw returns the raw name, Format().Size() bytes long. The slice is a copy.
func (id ID) Raw() []byte { return bytes.Clone(id.raw[:id.format.Size()]) }

// String returns the lower-case hex name, "" for the zero ID.
func (id ID) String() string {
	if id.format == 0 {
		return ""
	}
	return hex.EncodeToString(id.raw[:id.format.Size()])
}

// IsZero reports the null name of any format, or the zero ID.
func (id ID) IsZero() bool { return id.raw == [sha256.Size]byte{} }

// Compare orders names bytewise, the order of pack indexes and loose fan-out.
func (id ID) Compare(other ID) int { return bytes.Compare(id.raw[:], other.raw[:]) }

// Kind is an object type, numbered as in pack entry headers.
type Kind uint8

const (
	Commit Kind = 1
	Tree   Kind = 2
	Blob   Kind = 3
	Tag    Kind = 4
)

// ParseKind maps an object header's type name to its Kind.
func ParseKind(name string) (Kind, error) {
	switch name {
	case "commit":
		return Commit, nil
	case "tree":
		return Tree, nil
	case "blob":
		return Blob, nil
	case "tag":
		return Tag, nil
	}
	return 0, fmt.Errorf("unknown object type %q", name)
}

// String returns the type name used in object headers and by git cat-file.
func (k Kind) String() string {
	switch k {
	case Commit:
		return "commit"
	case Tree:
		return "tree"
	case Blob:
		return "blob"
	case Tag:
		return "tag"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Hash returns the name of an object of kind k with content data in format f:
// the hash of "<type> <size>\x00" followed by data.
func Hash(f Format, k Kind, data []byte) ID {
	h := f.New()
	h.Write(Header(k, int64(len(data))))
	h.Write(data)
	id := ID{format: f}
	h.Sum(id.raw[:0])
	return id
}

// Header is the "<type> <size>\x00" prefix hashed before an object's content
// and stored, compressed with it, in a loose object.
func Header(k Kind, size int64) []byte {
	b := append([]byte(k.String()), ' ')
	b = strconv.AppendInt(b, size, 10)
	return append(b, 0)
}

// Mode is a tree or index entry's file mode in git's octal encoding.
type Mode uint32

const (
	ModeTree    Mode = 0o040000
	ModeFile    Mode = 0o100644
	ModeExec    Mode = 0o100755
	ModeSymlink Mode = 0o120000
	ModeGitlink Mode = 0o160000 // a submodule commit
)

// Kind returns the kind of object an entry of mode m names: a tree, a commit
// for a gitlink, else a blob.
func (m Mode) Kind() Kind {
	switch m {
	case ModeTree:
		return Tree
	case ModeGitlink:
		return Commit
	}
	return Blob
}

// String returns the six-digit octal spelling git prints ("100644", "040000").
func (m Mode) String() string {
	s := strconv.FormatUint(uint64(m), 8)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

// TreeEntry is one entry of a tree object: a path component, its mode and the
// object it names.
type TreeEntry struct {
	Name string
	Mode Mode
	ID   ID
}
