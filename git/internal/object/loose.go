package object

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// loosePath returns where a loose object id lives under objects directory dir.
func loosePath(dir string, id ID) string {
	h := id.String()
	return filepath.Join(dir, h[:2], h[2:])
}

// looseHeader reads the "<type> <size>\0" prefix of a loose object from its
// decompressed stream.
func looseHeader(z *bufio.Reader) (Kind, int64, error) {
	hdr, err := z.ReadSlice(0)
	if err != nil {
		return 0, 0, fmt.Errorf("malformed header: %w", err)
	}
	name, num, ok := bytes.Cut(hdr[:len(hdr)-1], []byte{' '})
	if !ok {
		return 0, 0, fmt.Errorf("malformed header %q", hdr)
	}
	kind, err := ParseKind(string(name))
	if err != nil {
		return 0, 0, err
	}
	size, err := strconv.ParseInt(string(num), 10, 64)
	if err != nil || size < 0 || (len(num) > 1 && num[0] == '0') {
		return 0, 0, fmt.Errorf("malformed size in header %q", hdr)
	}
	return kind, size, nil
}

// openLoose opens a loose object and decodes its header, leaving the
// returned reader positioned at the content.
func openLoose(path string) (_ *os.File, _ io.ReadCloser, _ *bufio.Reader, kind Kind, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, 0, 0, err
	}
	z, err := openZlib(bufio.NewReader(f))
	if err != nil {
		f.Close()
		return nil, nil, nil, 0, 0, fmt.Errorf("loose object %s: zlib: %w", path, err)
	}
	// Headers are short; the bounded buffer also bounds the search for the NUL.
	br := bufio.NewReaderSize(z, 64)
	kind, size, err = looseHeader(br)
	if err != nil {
		closeZlib(z)
		f.Close()
		return nil, nil, nil, 0, 0, fmt.Errorf("loose object %s: %w", path, err)
	}
	return f, z, br, kind, size, nil
}

func readLooseHeader(path string) (Kind, int64, error) {
	f, z, _, kind, size, err := openLoose(path)
	if err != nil {
		return 0, 0, err
	}
	closeZlib(z)
	f.Close()
	return kind, size, nil
}

func readLoose(path string) (Kind, []byte, error) {
	f, z, br, kind, size, err := openLoose(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	defer closeZlib(z)
	if fi, err := f.Stat(); err == nil && size > fi.Size()*maxDeflateRatio+64 {
		return 0, nil, fmt.Errorf("loose object %s: header claims impossible size %d", path, size)
	}
	data, err := readSized(br, size)
	if err != nil {
		return 0, nil, fmt.Errorf("loose object %s: content shorter than header size %d: %w", path, size, err)
	}
	if err := expectEnd(br); err != nil {
		return 0, nil, fmt.Errorf("loose object %s: %w", path, err)
	}
	return kind, data, nil
}

// DecodeLoose decodes a zlib-compressed loose object read from r, such as one
// a dumb HTTP server sends. An object whose header declares more than maxSize
// bytes (when maxSize > 0) is refused before its content is read; content
// shorter or longer than the header declares is an error.
func DecodeLoose(r io.Reader, maxSize int64) (Kind, []byte, error) {
	z, err := openZlib(r)
	if err != nil {
		return 0, nil, fmt.Errorf("loose object: zlib: %w", err)
	}
	defer closeZlib(z)
	br := bufio.NewReaderSize(z, 64)
	kind, size, err := looseHeader(br)
	if err != nil {
		return 0, nil, fmt.Errorf("loose object: %w", err)
	}
	if maxSize > 0 && size > maxSize {
		return 0, nil, fmt.Errorf("loose object: %s of %d bytes is over the %d-byte object size limit", kind, size, maxSize)
	}
	data, err := readSized(br, size)
	if err != nil {
		return 0, nil, fmt.Errorf("loose object: content shorter than header size %d: %w", size, err)
	}
	if err := expectEnd(br); err != nil {
		return 0, nil, fmt.Errorf("loose object: %w", err)
	}
	return kind, data, nil
}

// WriteLoose stores an object as a zlib-compressed loose object in the
// database's own objects directory and returns its name. An object already
// present anywhere in the database is not rewritten, only freshened (its file
// time updated) so that pruning sees it as recent, as git does.
func (db *DB) WriteLoose(kind Kind, data []byte) (ID, error) {
	if db.closed.Load() {
		return ID{}, errClosed
	}
	id := Hash(db.format, kind, data)
	if db.freshen(id) {
		return id, nil
	}
	path := loosePath(db.dirs[0].path, id)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return ID{}, err
	}
	// Loose objects are read-only, as git creates them.
	tmp, err := createTemp(dir, "tmp_obj_", 0o444)
	if err != nil {
		return ID{}, err
	}
	bw := bufio.NewWriter(tmp)
	zw, _ := zlib.NewWriterLevel(bw, zlib.BestSpeed) // git's core.looseCompression default
	zw.Write(Header(kind, int64(len(data))))
	zw.Write(data)
	err = zw.Close()
	if err == nil {
		err = bw.Flush()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = publish(tmp.Name(), path)
	}
	if err != nil {
		removeTemp(tmp.Name())
		return ID{}, fmt.Errorf("writing loose object %s: %w", id, err)
	}
	return id, nil
}

// freshen reports whether id is already stored, updating the time of the
// file holding it.
func (db *DB) freshen(id ID) bool {
	now := time.Now()
	for _, d := range db.dirs {
		if p := loosePath(d.path, id); os.Chtimes(p, now, now) == nil {
			return true
		}
		for _, p := range *d.packs.Load() {
			if !p.acquire() {
				continue
			}
			_, ok, _ := p.idx.find(id)
			p.release()
			if ok {
				os.Chtimes(p.path, now, now)
				return true
			}
		}
	}
	return false
}
