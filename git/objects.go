package git

import (
	"fmt"

	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
)

// Objects reads a repository's objects, alternates included, honouring replace
// refs as git does. It is safe for concurrent use and keeps pack files open
// until Close.
type Objects struct {
	format object.Format
	db     *repo.Objects
}

// Objects opens r's object database for reading.
func (r *Repo) Objects() (*Objects, error) {
	db, err := r.repo.OpenObjects()
	if err != nil {
		return nil, err
	}
	return &Objects{format: r.repo.Format, db: db}, nil
}

// id parses a full hex object name of the repository's format.
func (o *Objects) id(oid string) (object.ID, error) {
	id, err := object.ParseIDFormat(o.format, oid)
	if err != nil {
		return object.ID{}, fmt.Errorf("invalid %s object name %q", o.format, oid)
	}
	return id, nil
}

// Blob returns a blob's bytes. A missing object is an error matching os.ErrNotExist.
func (o *Objects) Blob(oid string) ([]byte, error) {
	id, err := o.id(oid)
	if err != nil {
		return nil, err
	}
	kind, data, err := o.db.Read(id)
	if err != nil {
		return nil, fmt.Errorf("git object %s: %w", oid, err)
	}
	if kind != object.Blob {
		return nil, fmt.Errorf("git object %s is %s, not blob", oid, kind)
	}
	return data, nil
}

// Size returns an object's size without reading its content. A missing object
// (such as a blob a partial clone never fetched) is an error matching
// os.ErrNotExist; nothing is fetched.
func (o *Objects) Size(oid string) (int64, error) {
	id, err := o.id(oid)
	if err != nil {
		return 0, err
	}
	_, size, err := o.db.Header(id)
	if err != nil {
		return 0, fmt.Errorf("git object %s: %w", oid, err)
	}
	return size, nil
}

// Close releases the database's files; later reads fail.
func (o *Objects) Close() error { return o.db.Close() }
