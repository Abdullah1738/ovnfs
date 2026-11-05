// Package clone copies regular files by sharing their data blocks where the
// filesystem supports it (clonefile on APFS, FICLONE reflinks on Linux) and by
// copying bytes everywhere else, so callers get one portable copy primitive.
// Reflink alone reports errors.ErrUnsupported, leaving dst absent, where blocks
// cannot be shared, for callers with a cheaper fallback than a plain copy.
package clone

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// File clones the regular file at src to the new path dst, preserving its permission bits.
func File(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("clone %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("clone %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("clone %s: not a regular file", src)
	}
	return FD(in, dst, info.Mode().Perm())
}

// FD clones the open regular file in to the new path dst (O_EXCL) with mode,
// copying bytes from the open handle when the filesystem cannot clone.
func FD(in *os.File, dst string, mode os.FileMode) error {
	err := Reflink(in, dst, mode)
	if errors.Is(err, errors.ErrUnsupported) {
		return copyBytes(in, dst, mode)
	}
	return err
}

// copyBytes creates dst exclusively and copies all of in from offset zero, so
// the result matches a clone regardless of the descriptor's current offset.
func copyBytes(in *os.File, dst string, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("clone to %s: %w", dst, err)
	}
	_, err = io.Copy(out, io.NewSectionReader(in, 0, math.MaxInt64))
	if err == nil {
		err = out.Chmod(mode)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("copy to %s: %w", dst, err)
	}
	return nil
}
