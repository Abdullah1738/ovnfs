//go:build !darwin

package clone

import "io/fs"

// cloneHierarchy reports false: only APFS clones a hierarchy in one call, so
// [Tree] clones file by file here.
func cloneHierarchy(_, _ string, _ fs.FileInfo) (bool, error) { return false, nil }
