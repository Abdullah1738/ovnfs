package fsys

import _ "unsafe" // for go:cgo_import_dynamic

// arm64 has only the 64-bit-inode stat ABI, under the plain name.
//
//go:cgo_import_dynamic libc_fstatat fstatat "/usr/lib/libSystem.B.dylib"
