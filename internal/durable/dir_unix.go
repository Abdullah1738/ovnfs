//go:build !windows

package durable

import "os"

// Dir flushes the directory entry changes (creates, renames) in dir.
func Dir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return Sync(d)
}
