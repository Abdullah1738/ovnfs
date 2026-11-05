//go:build !unix

package unixstat

// native finds no POSIX metadata where the platform keeps none.
func native(any) (Info, bool) { return Info{}, false }
