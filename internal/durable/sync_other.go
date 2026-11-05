//go:build !darwin

package durable

import "os"

// Sync flushes f to stable storage.
func Sync(f *os.File) error {
	return f.Sync()
}
