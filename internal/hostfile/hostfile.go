// Package hostfile opens, renames and removes host files with POSIX semantics
// on every platform. On Unix it is the os package. On Windows, files open with
// FILE_SHARE_DELETE and renames and removes use POSIX semantics, so a file the
// daemon holds open for a client can still be renamed, replaced or deleted, and
// read-only files are replaced or deleted without touching their attribute, as
// on Unix.
// Windows still refuses to rename a directory while any file below it is open;
// see [overlay.FS.CloseFiles].
package hostfile

import "os"

// Open opens name for reading; see [OpenFile].
func Open(name string) (*os.File, error) { return OpenFile(name, os.O_RDONLY, 0) }
