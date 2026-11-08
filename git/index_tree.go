package git

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
)

// IndexTree reads the stage-zero entries of index file name, sparse directories
// expanded as `git ls-files --stage` lists them, with blob sizes from the object
// database and without reading blob content. A missing index is empty; a blob
// missing from the database is an error.
func (r *Repo) IndexTree(ctx context.Context, name string) ([]TreeEntry, error) {
	objects, err := r.repo.OpenObjects()
	if err != nil {
		return nil, err
	}
	defer objects.Close()
	idx, err := index.Read(name, r.repo.Format, index.ReadOptions{ExpandSparse: treeReader(objects)})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	var entries []TreeEntry
	for _, e := range idx.Entries {
		if e.Stage != 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := TreeEntry{Mode: uint32(e.Mode), Type: e.Mode.Kind().String(), OID: e.ID.String(), Path: e.Path}
		if e.Mode != object.ModeGitlink {
			if _, entry.Size, err = objects.Header(e.ID); err != nil {
				return nil, fmt.Errorf("index object %s: %w", e.ID, err)
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// treeReader reads tree objects from objects for the index engine.
func treeReader(objects *repo.Objects) index.TreeReader {
	return func(id object.ID) ([]object.TreeEntry, error) {
		kind, data, err := objects.Read(id)
		if err != nil {
			return nil, err
		}
		if kind != object.Tree {
			return nil, fmt.Errorf("object %s is a %s, not a tree", id, kind)
		}
		return object.ParseTree(id.Format(), data)
	}
}
