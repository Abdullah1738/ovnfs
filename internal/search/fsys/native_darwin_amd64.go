package fsys

import _ "unsafe" // for go:cgo_import_dynamic

// On amd64 the plain fstatat keeps the legacy 32-bit-inode struct stat;
// fstatat64 fills the layout unix.Stat_t describes.
//
//go:cgo_import_dynamic libc_fstatat fstatat64 "/usr/lib/libSystem.B.dylib"
