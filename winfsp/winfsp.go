// Package winfsp serves the export root as a Windows drive through WinFsp
// (https://winfsp.dev), a user-mode filesystem driver installed once per
// machine: the root lists checkouts read-only and each checkout below it is
// fully writable. Files look as they do to Windows SMB clients of the smb
// package (see ntinfo), and the drive is a network volume (\\ovnfs\O) as the
// SMB share is, but no SMB or port is involved, so the Server service can keep
// port 445.
package winfsp

import "time"

// TTL bounds how long the driver caches file and directory metadata; it is
// short because a checkout's overlay changes underneath on snapshot, fork, and
// removal. File data is never cached by the driver.
const TTL = time.Second
