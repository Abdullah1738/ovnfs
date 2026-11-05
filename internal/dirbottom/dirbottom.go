// Package dirbottom provides a live host-directory bottom for overlay tests.
package dirbottom

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/overlay"
)

// bottom exposes ordinary host entries without interpreting overlay whiteouts.
type bottom struct{ dir string }

// New returns a read-only overlay view of dir; mutations occur only through CopyTo's destination.
func New(dir string) overlay.Bottom { return bottom{dir: dir} }

// Lstat returns metadata without following the final symlink.
func (b bottom) Lstat(p string) (os.FileInfo, error) {
	return os.Lstat(filepath.Join(b.dir, filepath.FromSlash(p)))
}

// ReadDir returns children in name order, retaining symlink metadata.
func (b bottom) ReadDir(p string) ([]os.FileInfo, error) {
	entries, err := os.ReadDir(filepath.Join(b.dir, filepath.FromSlash(p)))
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// Open opens a regular file read-only; symlink resolution belongs to the overlay.
func (b bottom) Open(p string) (overlay.Content, error) {
	name := filepath.Join(b.dir, filepath.FromSlash(p))
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	f, err := hostfile.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Readlink returns the stored target without resolving it against the host
// directory, slash-separated as Windows hosts do not store it.
func (b bottom) Readlink(p string) (string, error) {
	target, err := os.Readlink(filepath.Join(b.dir, filepath.FromSlash(p)))
	return filepath.ToSlash(target), err
}

// CopyTo materializes an entry at a new host path, preserving permissions and modification times.
func (b bottom) CopyTo(p, dst string) error {
	return copyEntry(filepath.Join(b.dir, filepath.FromSlash(p)), dst)
}

// copyEntry copies regular files, symlinks, and directory trees without following symlinks.
func copyEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		return linkTime(dst, info.ModTime())
	case mode.IsDir():
		if err := os.Mkdir(dst, mode.Perm()|0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyEntry(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		if err := os.Chmod(dst, mode); err != nil {
			return err
		}
	case mode.IsRegular():
		if err := copyFile(src, dst, mode); err != nil {
			return err
		}
	default:
		return &os.PathError{Op: "copy", Path: src, Err: fmt.Errorf("unsupported file type %s: %w", mode.Type(), os.ErrInvalid)}
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// copyFile copies bytes and restores the mode after creation so the process umask cannot change it.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
