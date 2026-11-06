//go:build !unix && !windows

package overlay

import "os"

// encodeSys keeps no native record where the platform reports none.
func encodeSys(os.FileInfo) ([]byte, error) { return nil, nil }

// decodeSys restores no native record on this platform.
func decodeSys([]byte) (any, error) { return nil, nil }
