package store

import (
	"errors"
	"fmt"
	"hash"
	"os"
)

// BlobWriter receives a blob's bytes from elsewhere (a peer) and publishes it
// only if its content hashes to the expected id, so a store never holds an
// object under the wrong name. A blob of up to cloneFile bytes is gathered in
// memory and packed on commit (becoming resident in the cache where it fits);
// a larger one is staged privately and committed as content-defined chunks.
type BlobWriter struct {
	s     *Store
	stage string   // staging file of a loose blob, else ""
	f     *os.File // open stage, else nil
	h     hash.Hash
	body  []byte // received bytes of a packed blob, else nil
	size  int64
	n     int64
}

// NewBlobWriter stages a blob of the given size.
func (s *Store) NewBlobWriter(size int64) (*BlobWriter, error) {
	if size < 0 {
		return nil, errors.New("negative blob size")
	}
	w := &BlobWriter{s: s, h: newHash("blob", size), size: size}
	if size <= cloneFile {
		w.body = make([]byte, 0, size)
		return w, nil
	}
	w.stage = s.stageName()
	f, err := os.OpenFile(w.stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, objectPerm)
	if err != nil {
		s.staging.Delete(w.stage)
		return nil, err
	}
	w.f = f
	return w, nil
}

// Write appends content; writing past the announced size is an error.
func (w *BlobWriter) Write(p []byte) (int, error) {
	if w.n+int64(len(p)) > w.size {
		return 0, fmt.Errorf("blob longer than its announced %d bytes", w.size)
	}
	if w.f == nil {
		w.body = append(w.body, p...)
		w.h.Write(p)
		w.n += int64(len(p))
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.h.Write(p[:n])
	w.n += int64(n)
	return n, err
}

// Commit publishes the blob if it is complete and its content hashes to id.
func (w *BlobWriter) Commit(id OID) error {
	if w.f == nil {
		switch {
		case w.n != w.size:
			return fmt.Errorf("blob %s: got %d of %d bytes", id, w.n, w.size)
		case sumOID(w.h) != id:
			return fmt.Errorf("blob %s: content hashes to %s", id, sumOID(w.h))
		}
		var keep []byte
		if w.s.cache.fits(w.size) {
			keep = w.body
		}
		return w.s.putObject(id, w.body, keep, false)
	}
	defer w.s.staging.Delete(w.stage)
	closeErr := w.f.Close()
	switch {
	case closeErr != nil:
		removeFile(w.stage)
		return closeErr
	case w.n != w.size:
		removeFile(w.stage)
		return fmt.Errorf("blob %s: got %d of %d bytes", id, w.n, w.size)
	case sumOID(w.h) != id:
		removeFile(w.stage)
		return fmt.Errorf("blob %s: content hashes to %s", id, sumOID(w.h))
	}
	return w.s.chunkStage(id, w.stage)
}

// Abort discards a partially received blob.
func (w *BlobWriter) Abort() {
	if w.f == nil {
		w.body = nil
		return
	}
	w.f.Close()
	removeFile(w.stage)
	w.s.staging.Delete(w.stage)
}
