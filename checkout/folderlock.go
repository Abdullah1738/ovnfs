package checkout

import (
	"context"
	"sync"
)

// folderLocks admits one apply at a time into a source folder, or into any
// folder nested in or around it, so applies of several checkouts merge
// against each other's results instead of the same old content (each reads,
// merges and renames over the folder's files). The zero value is ready.
type folderLocks struct {
	mu   sync.Mutex
	held map[string]chan struct{} // mu: folders being written, each closed on release
}

// lock waits until no held folder contains root or lies inside it, then holds
// root until the returned release is called. It fails with ctx's error,
// coded, when ctx ends first.
func (l *folderLocks) lock(ctx context.Context, root string) (release func(), err error) {
	for {
		l.mu.Lock()
		busy := l.overlapLocked(root)
		if busy == nil {
			if l.held == nil {
				l.held = map[string]chan struct{}{}
			}
			done := make(chan struct{})
			l.held[root] = done
			l.mu.Unlock()
			return func() {
				l.mu.Lock()
				delete(l.held, root)
				l.mu.Unlock()
				close(done)
			}, nil
		}
		l.mu.Unlock()
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, coded(ctx.Err())
		}
	}
}

// overlapLocked returns the release channel of a held folder that is root,
// contains it or lies inside it; nil when none does.
func (l *folderLocks) overlapLocked(root string) chan struct{} {
	for held, done := range l.held {
		if inside(held, root) || inside(root, held) {
			return done
		}
	}
	return nil
}
