package repo

import (
	"fmt"
	"iter"
	"os"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

const (
	// maxReplaceDepth bounds chains of replace refs (A replaced by B, B by C).
	maxReplaceDepth = 5
	// maxPeel bounds tag chains, which only corruption could make cyclic.
	maxPeel = 1000
)

// Objects reads a repository's objects with replace refs applied
// (git-replace): reading an object named by refs/replace/<id> yields the
// replacement's kind and content instead. Has, FindPrefix, Walk and DiffTrees
// see the stored objects. It is safe for concurrent use; Close releases it.
type Objects struct {
	db *object.DB
	// replace maps replaced objects to their replacements; nil when none
	// apply.
	replace map[object.ID]object.ID
}

// OpenObjects opens the repository's objects for reading with replace refs
// applied, unless GIT_NO_REPLACE_OBJECTS is set (to any value) or
// core.useReplaceRefs is false. Without replace refs it costs one listing of
// refs/replace/. The caller closes the result.
func (r *Repository) OpenObjects() (*Objects, error) {
	replace, err := r.replaceRefs()
	if err != nil {
		return nil, err
	}
	db, err := r.OpenDB()
	if err != nil {
		return nil, err
	}
	return &Objects{db: db, replace: replace}, nil
}

// replaceRefs reads refs/replace/, ignoring names that are not a full object
// name (git warns about those), or returns nil when replacement is off.
func (r *Repository) replaceRefs() (map[object.ID]object.ID, error) {
	list, err := r.refs.List("refs/replace/")
	if err != nil || len(list) == 0 {
		return nil, err
	}
	if _, off := os.LookupEnv("GIT_NO_REPLACE_OBJECTS"); off {
		return nil, nil
	}
	cfg, err := r.Config()
	if err != nil {
		return nil, err
	}
	if use, ok, err := cfg.Bool("core.useReplaceRefs"); err != nil {
		return nil, err
	} else if ok && !use {
		return nil, nil
	}
	replace := map[object.ID]object.ID{}
	for _, ref := range list {
		from, err := object.ParseIDFormat(r.Format, strings.TrimPrefix(ref.Name, "refs/replace/"))
		if err != nil {
			continue
		}
		to := ref.ID
		if ref.Symbolic() {
			resolved, err := r.refs.Resolve(ref.Name)
			if err != nil {
				continue
			}
			to = resolved.ID
		}
		replace[from] = to
	}
	if len(replace) == 0 {
		return nil, nil
	}
	return replace, nil
}

// Close releases the underlying object database.
func (o *Objects) Close() error { return o.db.Close() }

// Format returns the repository's object hash format.
func (o *Objects) Format() object.Format { return o.db.Format() }

// Replaced reports whether at least one replace ref applies.
func (o *Objects) Replaced() bool { return o.replace != nil }

// Replacement returns the object read in place of id: id itself unless a
// replace ref (or a chain of them) applies.
func (o *Objects) Replacement(id object.ID) (object.ID, error) {
	if o.replace == nil {
		return id, nil
	}
	start := id
	for range maxReplaceDepth {
		next, ok := o.replace[id]
		if !ok {
			return id, nil
		}
		id = next
	}
	if _, ok := o.replace[id]; !ok {
		return id, nil
	}
	return object.ID{}, fmt.Errorf("replace depth too high for object %s", start)
}

// Read returns the kind and content of id, or of its replacement.
func (o *Objects) Read(id object.ID) (object.Kind, []byte, error) {
	target, err := o.Replacement(id)
	if err != nil {
		return 0, nil, err
	}
	return o.db.Read(target)
}

// Header returns the kind and size of id, or of its replacement.
func (o *Objects) Header(id object.ID) (object.Kind, int64, error) {
	target, err := o.Replacement(id)
	if err != nil {
		return 0, 0, err
	}
	return o.db.Header(target)
}

// Has reports whether the database stores id itself.
func (o *Objects) Has(id object.ID) bool { return o.db.Has(id) }

// FindPrefix lists stored objects whose hex name starts with hexPrefix (see
// object.DB.FindPrefix).
func (o *Objects) FindPrefix(hexPrefix string, limit int) ([]object.ID, error) {
	return o.db.FindPrefix(hexPrefix, limit)
}

// Dirs lists the object directories searched, own first, then alternates.
func (o *Objects) Dirs() []string { return o.db.Dirs() }

// Peel follows annotated tags from id until an object of kind want, reading
// through replacements; for want Tree a commit yields its tree. The result
// names the object reached (not its replacement), as git rev-parse prints.
func (o *Objects) Peel(id object.ID, want object.Kind) (object.ID, error) {
	if o.replace == nil {
		return o.db.Peel(id, want)
	}
	f := o.Format()
	for range maxPeel {
		kind, _, err := o.Header(id)
		if err != nil {
			return object.ID{}, err
		}
		if kind == want {
			return id, nil
		}
		if kind != object.Tag && (kind != object.Commit || want != object.Tree) {
			return object.ID{}, fmt.Errorf("object %s is a %s, not a %s", id, kind, want)
		}
		_, data, err := o.Read(id)
		if err != nil {
			return object.ID{}, err
		}
		if kind == object.Tag {
			t, err := object.ParseTag(f, data)
			if err != nil {
				return object.ID{}, fmt.Errorf("tag %s: %w", id, err)
			}
			id = t.Object
			continue
		}
		c, err := object.ParseCommit(f, data)
		if err != nil {
			return object.ID{}, fmt.Errorf("commit %s: %w", id, err)
		}
		return c.Tree, nil
	}
	return object.ID{}, fmt.Errorf("object %s: tag chain too long", id)
}

// Walk yields the entries below root recursively, without replacements (see
// object.DB.Walk).
func (o *Objects) Walk(root object.ID, opts object.WalkOptions) iter.Seq2[object.TreeEntry, error] {
	return o.db.Walk(root, opts)
}

// DiffTrees compares two trees without replacements (see object.DB.DiffTrees).
func (o *Objects) DiffTrees(a, b object.ID) ([]object.Change, error) {
	return o.db.DiffTrees(a, b)
}
