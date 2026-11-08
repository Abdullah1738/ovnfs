package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

// Revision is a commit resolved before a checkout is created. Branch is nonempty
// only when the requested revision names a local branch, not HEAD or a tag.
type Revision struct {
	Commit, Branch string
	Entries        []TreeEntry
	repo           *Repo
}

// ResolveRevision validates spec, any git revision naming a commit, without
// changing the repository.
func (r *Repo) ResolveRevision(ctx context.Context, spec string) (*Revision, error) {
	v := &Revision{repo: r}
	err := r.session(func(s *rev.Source) error {
		id, err := rev.Resolve(s, spec+"^{commit}", rev.Options{EndOfOptions: true})
		if err != nil {
			return err
		}
		v.Commit = id.String()
		branch := spec
		if !strings.HasPrefix(branch, "refs/heads/") {
			branch = "refs/heads/" + branch
		}
		if tip, err := s.Refs.Resolve(branch); err == nil && tip.ID == id {
			v.Branch = branch
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if v.Entries, err = r.lsTree(ctx, v.Commit, true); err != nil {
		return nil, err
	}
	return v, nil
}

// WriteIndex writes the index `git read-tree` makes of the revision to the file
// name, a private index; it never checks out or copies files.
func (v *Revision) WriteIndex(ctx context.Context, name string) error {
	objects, err := v.repo.repo.OpenObjects()
	if err != nil {
		return err
	}
	defer objects.Close()
	commit, err := object.ParseIDFormat(v.repo.repo.Format, v.Commit)
	if err != nil {
		return err
	}
	tree, err := objects.Peel(commit, object.Tree)
	if err != nil {
		return err
	}
	idx, err := index.FromTree(treeReader(objects), tree, v.repo.repo.Format)
	if err != nil {
		return err
	}
	return index.Write(name, idx, index.WriteOptions{})
}

// SetHEAD attaches a private git directory to a local branch, or detaches it at
// the resolved commit. It never updates the source repository's refs.
func (v *Revision) SetHEAD(gitdir string) error {
	head := v.Commit
	if v.Branch != "" {
		head = "ref: " + v.Branch
		// Refs were copied separately from revision resolution. Pin the private
		// branch to the resolved commit even if the source moved meanwhile.
		name := filepath.Join(gitdir, filepath.FromSlash(v.Branch))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(name, []byte(v.Commit+"\n"), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte(head+"\n"), 0o644)
}
