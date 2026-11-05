package store

import (
	"errors"
	"fmt"
	"hash"
	"os"
)

// BlobWriter receives a blob's bytes from elsewhere (a peer) and publishes it
// only if its content hashes to the expected id, so a store never holds an
// object under the wrong name. A small blob is also kept to become resident
// in the cache on commit.
type BlobWriter struct {
	s     *Store
	stage string
	f     *os.File
	h     hash.Hash
	body  []byte // received bytes when the blob fits the cache, else nil
	size  int64
	n     int64
}

// NewBlobWriter stages a blob of the given size.
func (s *Store) NewBlobWriter(size int64) (*BlobWriter, error) {
	if size < 0 {
		return nil, errors.New("negative blob size")
	}
	stage := s.stageName()
	f, err := os.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, objectPerm)
	if err != nil {
		s.staging.Delete(stage)
		return nil, err
	}
	w := &BlobWriter{s: s, stage: stage, f: f, h: newHash("blob", size), size: size}
	if s.cache.fits(size) {
		w.body = make([]byte, 0, size)
	}
	return w, nil
}

// Write appends content; writing past the announced size is an error.
func (w *BlobWriter) Write(p []byte) (int, error) {
	if w.n+int64(len(p)) > w.size {
		return 0, fmt.Errorf("blob longer than its announced %d bytes", w.size)
	}
	n, err := w.f.Write(p)
	w.h.Write(p[:n])
	if w.body != nil {
		w.body = append(w.body, p[:n]...)
	}
	w.n += int64(n)
	return n, err
}

// Commit publishes the blob if it is complete and its content hashes to id.
func (w *BlobWriter) Commit(id OID) error {
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
	return w.s.publish(id, w.stage, w.body, false)
}

// Abort discards a partially received blob.
func (w *BlobWriter) Abort() {
	w.f.Close()
	removeFile(w.stage)
	w.s.staging.Delete(w.stage)
}
