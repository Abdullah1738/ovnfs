package clone

import (
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// integrityInformation is FSCTL_GET_INTEGRITY_INFORMATION_BUFFER.
type integrityInformation struct {
	checksumAlgorithm uint16
	_                 uint16
	flags             uint32
	_                 uint32 // ChecksumChunkSizeInBytes
	clusterSize       uint32
}

// setIntegrityInformation is FSCTL_SET_INTEGRITY_INFORMATION_BUFFER.
type setIntegrityInformation struct {
	checksumAlgorithm uint16
	_                 uint16
	flags             uint32
}

// duplicateExtents is DUPLICATE_EXTENTS_DATA. Widening the HANDLE to 64 bits
// also gives 32-bit Windows the C layout, padded before the first offset.
type duplicateExtents struct {
	handle       uint64
	sourceOffset int64
	targetOffset int64
	byteCount    int64
}

// setSparseBuffer is FILE_SET_SPARSE_BUFFER.
type setSparseBuffer struct{ sparse bool }

// maxRegion bounds one FSCTL_DUPLICATE_EXTENTS_TO_FILE call: under 4 GiB.
const maxRegion = 4<<30 - 1

// none passes no buffer to [fsctl].
var none *struct{}

// cloneInto block-clones in into the empty out, following the ReFS rules at
// https://learn.microsoft.com/windows/win32/fileio/block-cloning: each region
// is cluster-aligned and under 4 GiB, out is extended to in's length first, and
// out matches in's integrity-stream and sparse settings. The last region is
// rounded up to the whole cluster holding the end of file, as the reflink tools
// do. On failure out may be extended and sparse; [Reflink] removes it.
func cloneInto(out, in *os.File) error {
	// Filesystems without block cloning, NTFS among them, fail here.
	var integrity integrityInformation
	if err := fsctl(in, windows.FSCTL_GET_INTEGRITY_INFORMATION, none, &integrity); err != nil {
		return err
	}
	info, err := in.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	cluster := max(int64(integrity.clusterSize), 1)
	// Best effort: some volumes (reportedly Dev Drive) refuse the change, and a
	// real mismatch then fails the clone itself.
	set := setIntegrityInformation{checksumAlgorithm: integrity.checksumAlgorithm, flags: integrity.flags}
	_ = fsctl(out, windows.FSCTL_SET_INTEGRITY_INFORMATION, &set, none)
	// Sparse while cloning, so extending the end of file allocates no clusters
	// the clone replaces anyway; ReFS also requires it of the destination of a
	// sparse source.
	if err := setSparse(out, true); err != nil {
		return err
	}
	if err := out.Truncate(size); err != nil {
		return err
	}
	limit := maxRegion / cluster * cluster
	end := (size + cluster - 1) / cluster * cluster
	for offset := int64(0); offset < end; {
		n := min(limit, end-offset)
		data := duplicateExtents{handle: uint64(in.Fd()), sourceOffset: offset, targetOffset: offset, byteCount: n}
		if err := fsctl(out, windows.FSCTL_DUPLICATE_EXTENTS_TO_FILE, &data, none); err != nil {
			return err
		}
		offset += n
	}
	if attrs, ok := info.Sys().(*syscall.Win32FileAttributeData); ok && attrs.FileAttributes&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
		return nil
	}
	return setSparse(out, false)
}

// setSparse sets or clears f's sparse attribute.
func setSparse(f *os.File, sparse bool) error {
	return fsctl(f, windows.FSCTL_SET_SPARSE, &setSparseBuffer{sparse: sparse}, none)
}

// fsctl issues the filesystem control code on f with the plain-data input and
// output buffers; [none] passes no buffer.
func fsctl[I, O any](f *os.File, code uint32, input *I, output *O) error {
	var returned uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), code,
		(*byte)(unsafe.Pointer(input)), uint32(unsafe.Sizeof(*input)),
		(*byte)(unsafe.Pointer(output)), uint32(unsafe.Sizeof(*output)),
		&returned, nil)
}

// unsupported reports clone failures that mean "this filesystem or pair of
// volumes cannot share blocks", the only cases that fall back to copying.
// ERROR_BLOCK_TOO_MANY_REFERENCES is ReFS's limit on clones of one block.
func unsupported(err error) bool {
	for _, code := range []windows.Errno{
		windows.ERROR_INVALID_FUNCTION,
		windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_NOT_SAME_DEVICE,
		windows.ERROR_INVALID_PARAMETER,
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_BLOCK_TOO_MANY_REFERENCES,
	} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}
