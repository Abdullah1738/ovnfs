package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sweep deletes objects not reachable from roots (trees walked recursively) last stored before before (a loose object's mtime, a packed one's append or re-store time), dropping them from the cache too; blobs referenced by trees but absent from the store are ignored. Returns the number removed.
// Packed objects are removed only from packs where removable objects make up
// at least a quarter of the bytes: such a pack is rewritten without them, its
// other frames copied as they are, and deleted once the copies are durable.
// Elsewhere unreachable packed objects stay stored (and uncounted) until a
// later sweep finds enough garbage beside them. The delta bases of every
// object still stored are kept too.
// A missing root or subtree (other than EmptyTree) is an error and nothing is
// removed, since its unknown children cannot be proven unreachable. Staging
// files older than before that no goroutine of this process owns (leftovers of
// a crash) are removed too but not counted. Objects published or re-stored
// during the sweep carry a fresh mtime and survive when before precedes the
// sweep, and so does everything reachable from a tree Build returned at or
// after before. Sweep waits for every Hold to be released, and a new epoch
// begins before the first object is removed.
func (s *Store) Sweep(roots []OID, before time.Time) (int, error) {
	s.holds.Lock()
	defer s.holds.Unlock()
	live := make(map[OID]struct{})
	for _, root := range roots {
		if err := s.mark(root, live); err != nil {
			return 0, fmt.Errorf("sweep: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Builds register under s.mu, so every root registered so far is seen here.
	s.rootsMu.Lock()
	var recent []OID
	for id, at := range s.roots {
		if at.Before(before) {
			delete(s.roots, id)
		} else {
			recent = append(recent, id)
		}
	}
	s.rootsMu.Unlock()
	for _, root := range recent {
		if err := s.mark(root, live); err != nil {
			return 0, fmt.Errorf("sweep: %w", err)
		}
	}
	s.packs.keepBases(live, before.UnixNano())
	removed := 0
	// begin starts a new epoch before the first removal.
	begin := func() error {
		if removed > 0 {
			return nil
		}
		if err := s.renewEpoch(); err != nil {
			return err
		}
		s.treesMu.Lock()
		clear(s.trees)
		s.treesMu.Unlock()
		return nil
	}
	dead, err := s.packs.removals(live, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("sweep: %w", err)
	}
	if len(dead) > 0 {
		if err := begin(); err != nil {
			return 0, fmt.Errorf("sweep: %w", err)
		}
		for _, id := range dead {
			s.cache.remove(id.oid())
		}
		removed = len(dead)
		if err := s.packs.remove(dead); err != nil {
			return removed, fmt.Errorf("sweep: %w", err)
		}
	}
	fanouts, err := os.ReadDir(s.objects)
	if err != nil {
		return 0, fmt.Errorf("sweep: %w", err)
	}
	for _, fan := range fanouts {
		if !fan.IsDir() || len(fan.Name()) != 2 {
			continue
		}
		dir := filepath.Join(s.objects, fan.Name())
		names, err := os.ReadDir(dir)
		if err != nil {
			return removed, fmt.Errorf("sweep: %w", err)
		}
		for _, e := range names {
			if strings.HasPrefix(e.Name(), stagePrefix) {
				if err := s.sweepStage(filepath.Join(dir, e.Name()), before); err != nil {
					return removed, fmt.Errorf("sweep: %w", err)
				}
				continue
			}
			id := OID(fan.Name() + e.Name())
			if !id.Valid() {
				continue
			}
			if _, ok := live[id]; ok {
				continue
			}
			ok, err := olderThan(filepath.Join(dir, e.Name()), before)
			if err != nil {
				return removed, fmt.Errorf("sweep: %w", err)
			}
			if !ok {
				continue
			}
			if err := begin(); err != nil {
				return removed, fmt.Errorf("sweep: %w", err)
			}
			if err := removeFile(filepath.Join(dir, e.Name())); err != nil {
				return removed, fmt.Errorf("sweep: %w", err)
			}
			s.cache.remove(id)
			removed++
		}
	}
	if err := s.sweepStaging(before); err != nil {
		return removed, fmt.Errorf("sweep: %w", err)
	}
	return removed, nil
}

// mark adds id and, for trees, everything below it to live. Roots and
// ModeDir entries are trees; other entries are blobs and are not read.
func (s *Store) mark(id OID, live map[OID]struct{}) error {
	if _, ok := live[id]; ok {
		return nil
	}
	entries, err := s.ReadTree(id)
	if err != nil {
		return err
	}
	live[id] = struct{}{}
	for _, e := range entries {
		if e.Mode != ModeDir {
			live[e.ID] = struct{}{}
			continue
		}
		if err := s.mark(e.ID, live); err != nil {
			return err
		}
	}
	return nil
}

// sweepStaging removes stale staging files of the staging directory.
func (s *Store) sweepStaging(before time.Time) error {
	names, err := os.ReadDir(s.tmp)
	if err != nil {
		return err
	}
	for _, e := range names {
		if err := s.sweepStage(filepath.Join(s.tmp, e.Name()), before); err != nil {
			return err
		}
	}
	return nil
}

// sweepStage removes staging file p if it is older than before and not owned by an in-flight put.
func (s *Store) sweepStage(p string, before time.Time) error {
	if _, busy := s.staging.Load(p); busy {
		return nil
	}
	ok, err := olderThan(p, before)
	if err != nil || !ok {
		return err
	}
	return removeFile(p)
}

// olderThan reports whether p was last modified before t; a vanished file is not old.
func olderThan(p string, t time.Time) (bool, error) {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.ModTime().Before(t), nil
}
