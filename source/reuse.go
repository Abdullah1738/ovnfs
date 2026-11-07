package source

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// reuseRecord is what only a fresh Capture knows about its Result: the digest of
// every walked entry's stat data, and the tree that walk produced.
type reuseRecord struct {
	walk [sha256.Size]byte
	tree store.OID
}

// reuseHash streams an identity into SHA-256; strings are length-prefixed and
// integers fixed-width, so distinct field sequences never encode alike.
type reuseHash struct {
	h   hash.Hash
	buf []byte
}

// newReuseHash starts an empty identity.
func newReuseHash() *reuseHash {
	return &reuseHash{h: sha256.New(), buf: make([]byte, 0, 32<<10)}
}

// int appends a fixed-width integer.
func (e *reuseHash) int(v uint64) {
	e.buf = binary.BigEndian.AppendUint64(e.buf, v)
	e.spill()
}

// string appends a length-prefixed string.
func (e *reuseHash) string(s string) {
	e.buf = binary.BigEndian.AppendUint64(e.buf, uint64(len(s)))
	e.buf = append(e.buf, s...)
	e.spill()
}

// spill hashes the buffered bytes once enough have accumulated.
func (e *reuseHash) spill() {
	if len(e.buf) >= 16<<10 {
		e.h.Write(e.buf)
		e.buf = e.buf[:0]
	}
}

// sum returns the digest of everything appended.
func (e *reuseHash) sum() [sha256.Size]byte {
	e.h.Write(e.buf)
	e.buf = e.buf[:0]
	var sum [sha256.Size]byte
	e.h.Sum(sum[:0])
	return sum
}

// stat appends one walked entry: its path, class and the stat data that git
// trees and Info do not retain (full mode; a directory's mtime and size).
func (e *reuseHash) stat(p string, class rules.Class, info os.FileInfo, key fileKey) {
	e.string(p)
	e.string(string(class))
	e.int(uint64(info.Mode()))
	e.int(uint64(info.ModTime().UnixNano()))
	e.int(uint64(info.Size()))
	e.int(key.ino)
}

// ReuseKey identifies an ordinary capture, or returns empty when safe reuse
// cannot be proven. Only a Result fresh from a stored Capture qualifies, before
// a revision or take rewrites it; one decoded from disk, or whose tree changed
// since, has no key.
func (r Result) ReuseKey() string {
	c := r.reuse
	if c == nil || r.Tree == "" || r.Tree != c.tree {
		return ""
	}
	if len(r.Warnings) != 0 || len(r.BlobRepo) != 0 || len(r.Repos) > 1 {
		return ""
	}
	// Equal trees and walked stat data imply equal Info and Roots, so neither is hashed again.
	e := newReuseHash()
	e.string(r.Root)
	e.string(string(r.Tree))
	e.buf = append(e.buf, c.walk[:]...)
	e.int(uint64(len(r.Repos)))
	if len(r.Repos) == 1 {
		captured := r.Repos[0]
		if captured.Path != "" || captured.Module != "" || captured.Index == "" {
			return ""
		}
		repo, err := git.Open(r.Root)
		if err != nil || repo.Top != r.Root || repo.GitDir != repo.CommonDir || repo.Busy() != "" {
			return ""
		}
		// The index is compared from its capture copy, never from a mutable checkout index.
		index, err := reuseDigest(captured.Index)
		if err != nil {
			return ""
		}
		e.string(captured.Head)
		e.string(index)
		// Match everything MakeGitdir copies from the live repository.
		meta := map[string]string{"gitdir": repo.GitDir, "commondir": repo.CommonDir}
		for _, p := range []string{"HEAD", "config", "config.worktree", "packed-refs", "shallow", "info/exclude", "refs"} {
			root := repo.CommonDir
			if p == "HEAD" || p == "config.worktree" {
				root = repo.GitDir
			}
			name := filepath.Join(root, filepath.FromSlash(p))
			if err := filepath.WalkDir(name, func(host string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					if errors.Is(walkErr, os.ErrNotExist) && host == name {
						meta[p] = "missing"
						return nil
					}
					return walkErr
				}
				rel, err := filepath.Rel(root, host)
				if err != nil {
					return err
				}
				if entry.IsDir() {
					meta[filepath.ToSlash(rel)] = "dir"
					return nil
				}
				digest, err := reuseDigest(host)
				meta[filepath.ToSlash(rel)] = digest
				return err
			}); err != nil {
				return ""
			}
		}
		keys := SortedPaths(meta)
		e.int(uint64(len(keys)))
		for _, k := range keys {
			e.string(k)
			e.string(meta[k])
		}
	}
	sum := e.sum()
	return hex.EncodeToString(sum[:])
}

// reuseDigest hashes only small git metadata; source content uses the captured tree.
func reuseDigest(name string) (string, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("unsupported git metadata type")
	}
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return info.Mode().String() + ":" + hex.EncodeToString(h.Sum(nil)), nil
}
