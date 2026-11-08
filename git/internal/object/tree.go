package object

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ParseTree decodes a tree object of format f into its entries, in stored
// order. Modes are canonicalized as git does when reading a tree (100664
// becomes 100644, unknown types become gitlinks). An entry without a mode,
// name or complete object name, or whose name is empty or contains '/', is an
// error.
func ParseTree(f Format, data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	size := f.Size()
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 {
			return nil, fmt.Errorf("malformed tree: entry without mode")
		}
		var mode Mode
		for _, c := range data[:sp] {
			if c < '0' || c > '7' || mode > 0o7777777 {
				return nil, fmt.Errorf("malformed tree: invalid mode %q", data[:sp])
			}
			mode = mode<<3 | Mode(c-'0')
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 {
			return nil, fmt.Errorf("malformed tree: unterminated entry name")
		}
		name := data[:nul]
		if len(name) == 0 || bytes.IndexByte(name, '/') >= 0 {
			return nil, fmt.Errorf("malformed tree: invalid entry name %q", name)
		}
		data = data[nul+1:]
		if len(data) < size {
			return nil, fmt.Errorf("malformed tree: truncated object name for %q", name)
		}
		e := TreeEntry{Name: string(name), Mode: canonicalMode(mode), ID: ID{format: f}}
		copy(e.ID.raw[:], data[:size])
		entries = append(entries, e)
		data = data[size:]
	}
	return entries, nil
}

// canonicalMode maps a stored mode to the one git uses after reading a tree:
// regular files keep only their owner-execute bit, and anything that is not a
// file, symlink or directory is treated as a gitlink.
func canonicalMode(m Mode) Mode {
	switch m & 0o170000 {
	case 0o100000:
		if m&0o100 != 0 {
			return ModeExec
		}
		return ModeFile
	case 0o120000:
		return ModeSymlink
	case 0o040000:
		return ModeTree
	}
	return ModeGitlink
}

// EncodeTree returns the tree object holding entries, sorted into git's tree
// order (see CompareEntries). The caller's slice is not reordered.
func EncodeTree(entries []TreeEntry) []byte {
	sorted := slices.SortedFunc(slices.Values(entries), CompareEntries)
	n := 0
	for _, e := range sorted {
		n += 8 + len(e.Name) + e.ID.format.Size()
	}
	b := make([]byte, 0, n)
	for _, e := range sorted {
		// Trees store modes without leading zeros: "40000", not "040000".
		b = strconv.AppendUint(b, uint64(e.Mode), 8)
		b = append(b, ' ')
		b = append(b, e.Name...)
		b = append(b, 0)
		b = append(b, e.ID.raw[:e.ID.format.Size()]...)
	}
	return b
}

// CompareEntries orders tree entries as git sorts them in a tree: bytewise by
// name, where a subtree's name compares as if followed by '/'.
func CompareEntries(a, b TreeEntry) int {
	return compareTreeNames(a.Name, a.Mode == ModeTree, b.Name, b.Mode == ModeTree)
}

func compareTreeNames(a string, aTree bool, b string, bTree bool) int {
	n := min(len(a), len(b))
	if c := strings.Compare(a[:n], b[:n]); c != 0 {
		return c
	}
	return cmp.Compare(nameByteAt(a, n, aTree), nameByteAt(b, n, bTree))
}

// nameByteAt is the byte at i of name as tree order sees it: past the end a
// tree continues with '/', anything else ends (0, which names cannot contain).
func nameByteAt(name string, i int, tree bool) byte {
	switch {
	case i < len(name):
		return name[i]
	case tree:
		return '/'
	}
	return 0
}
