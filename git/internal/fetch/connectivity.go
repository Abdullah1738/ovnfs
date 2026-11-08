package fetch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// ErrIncomplete reports objects missing after a fetch.
var ErrIncomplete = errors.New("fetch: missing objects after fetch")

// checkConnected verifies that every object reachable from wants is in db,
// not descending past shallow commits nor into complete commits (the
// target's ref tips, whose history is taken to be present).
// Commits, trees and tags over maxSize bytes (when > 0) are refused before
// they are read.
func checkConnected(ctx context.Context, db *object.DB, wants []object.ID, shallow, complete map[object.ID]struct{}, maxSize int64) error {
	// One rescan picks up packs written since the last; the many
	// existence checks below then never rescan on a miss.
	if err := db.Rescan(); err != nil {
		return err
	}
	seen := make(map[object.ID]struct{})
	blobs := make(map[object.ID]struct{}) // ids reached as blob tree entries
	stack := append([]object.ID(nil), wants...)
	f := db.Format()
	missing := func(id object.ID, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrIncomplete, id)
		}
		return fmt.Errorf("fetch: checking %s: %w", id, err)
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := complete[id]; ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !db.Contains(id) {
			return fmt.Errorf("%w: %s", ErrIncomplete, id)
		}
		if _, ok := blobs[id]; ok {
			continue
		}
		kind, size, err := db.Header(id)
		if err != nil {
			return missing(id, err)
		}
		if kind == object.Blob {
			continue
		}
		if maxSize > 0 && size > maxSize {
			return fmt.Errorf("fetch: %s %s is %d bytes, over the %d-byte object size limit", kind, id, size, maxSize)
		}
		_, data, err := db.Read(id)
		if err != nil {
			return missing(id, err)
		}
		switch kind {
		case object.Commit:
			c, err := object.ParseCommit(f, data)
			if err != nil {
				return fmt.Errorf("fetch: commit %s: %w", id, err)
			}
			stack = append(stack, c.Tree)
			if _, ok := shallow[id]; !ok {
				stack = append(stack, c.Parents...)
			}
		case object.Tree:
			entries, err := object.ParseTree(f, data)
			if err != nil {
				return fmt.Errorf("fetch: tree %s: %w", id, err)
			}
			for _, e := range entries {
				switch e.Mode.Kind() {
				case object.Blob:
					// The mode names the kind: existence is all to check.
					blobs[e.ID] = struct{}{}
					stack = append(stack, e.ID)
				case object.Tree:
					stack = append(stack, e.ID)
				}
			}
		case object.Tag:
			t, err := object.ParseTag(f, data)
			if err != nil {
				return fmt.Errorf("fetch: tag %s: %w", id, err)
			}
			stack = append(stack, t.Object)
		}
	}
	return nil
}

// checkParentsPresent requires commit id and all its parents in db.
func checkParentsPresent(db *object.DB, id object.ID) error {
	kind, data, err := db.Read(id)
	if err != nil {
		return err
	}
	if kind != object.Commit {
		return fmt.Errorf("%s is a %s, not a commit", id, kind)
	}
	c, err := object.ParseCommit(db.Format(), data)
	if err != nil {
		return err
	}
	for _, p := range c.Parents {
		if !db.Contains(p) {
			return fmt.Errorf("its parent %s is missing", p)
		}
	}
	return nil
}

// missingParents lists the commits among candidates that db has but whose
// parents it lacks: commits a shallow server sent as its own boundary.
func missingParents(db *object.DB, candidates []object.ID) []object.ID {
	db.Rescan() // see the pack just received; a failure only means misses
	var out []object.ID
	for _, id := range candidates {
		kind, data, err := db.Read(id)
		if err != nil || kind != object.Commit {
			continue
		}
		c, err := object.ParseCommit(db.Format(), data)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if !db.Contains(p) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}
