//go:build !(darwin && cgo) && !linux && !windows

package watch

import "errors"

// Watcher is unavailable on this platform; New always fails.
type Watcher struct{}

// New reports errors.ErrUnsupported: no change-notification backend exists here.
func New(fn func(paths []string)) (*Watcher, error) { return nil, errors.ErrUnsupported }

// Add reports errors.ErrUnsupported.
func (w *Watcher) Add(dir string) error { return errors.ErrUnsupported }

// Remove does nothing.
func (w *Watcher) Remove(dir string) {}

// Covers reports false.
func (w *Watcher) Covers(dir string) bool { return false }

// Close does nothing.
func (w *Watcher) Close() error { return nil }
