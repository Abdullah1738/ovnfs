package durable

// Dir is a no-op: Windows cannot flush a directory handle, and NTFS/ReFS
// journal renames and creates themselves.
func Dir(string) error { return nil }
