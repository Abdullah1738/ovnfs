//go:build !unix && !windows

package object

import (
	"errors"
	"os"
)

// mmap is unavailable here; mappedFile falls back to ReadAt.
func mmap(*os.File, int) ([]byte, error) { return nil, errors.ErrUnsupported }

func munmap([]byte) error { return nil }
