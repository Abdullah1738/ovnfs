package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Revision is a commit resolved before a checkout is created. Branch is nonempty
// only when the requested revision names a local branch, not HEAD or a tag.
type Revision struct {
	Commit, Branch string
	Entries        []TreeEntry
	repo           *Repo
}

// ResolveRevision validates rev without changing the repository.
func (r *Repo) ResolveRevision(ctx context.Context, rev string) (*Revision, error) {
	commit, err := r.text(ctx, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return nil, err
	}
	v := &Revision{Commit: commit, repo: r}
	branch := rev
	if !strings.HasPrefix(branch, "refs/heads/") {
		branch = "refs/heads/" + branch
	}
	if tip, err := r.text(ctx, "show-ref", "--verify", "--hash", branch); err == nil && tip == commit {
		v.Branch = branch
	}
	v.Entries, err = r.lsTree(ctx, commit, true)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// WriteIndex builds only a private index; it never checks out or copies files.
func (v *Revision) WriteIndex(ctx context.Context, name string) error {
	cmd := v.repo.command(ctx, "read-tree", v.Commit)
	cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+name)
	_, err := run(cmd)
	return err
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
