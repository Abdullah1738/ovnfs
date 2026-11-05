//go:build windows

package overlay

import "os"

// owner is "-": Windows files have no uid:gid.
func owner(os.FileInfo) string { return "-" }
