package base

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// revisionView evaluates ignore files from the revision, while testing the type
// of existing ignored paths against the folder. Dirty ignore files must not
// decide what survives a revision checkout.
type revisionView struct {
	source.Dir
	files map[string]store.File
	cat   *git.CatFile
}

func (v revisionView) ReadFile(p string) ([]byte, error) {
	f, ok := v.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return v.cat.Blob(string(f.ID))
}

// reviseCapture substitutes tree metadata, not a materialized checkout. The
// capture's stat identities remain usable only for content identical to REV.
func reviseCapture(ctx context.Context, c Config, res *source.Result, revision *git.Revision) error {
	index := filepath.Join(c.StateDir, "revision-index")
	if err := revision.WriteIndex(ctx, index); err != nil {
		return err
	}
	repos := []source.Repo{{Path: "", Head: revision.Commit, Index: index}}
	domains, err := source.NewDomains(res.Root, repos)
	if err != nil {
		return err
	}
	repo := domains.Repo("")
	cat, err := repo.CatFile()
	if err != nil {
		return err
	}
	defer cat.Close()
	files, info := treeFiles(revision.Entries, *res)
	tracked := map[string]bool{}
	for file := range files {
		for p := file; p != "."; p = path.Dir(p) {
			tracked[p] = true
		}
	}
	view := revisionView{Dir: source.Dir(res.Root), files: files, cat: cat}
	roots := map[string]rules.Class{}
	// Ignored roots remain live and copy-on-write, just as in an ordinary
	// checkout. Nonignored local/cache roots can contain ignored descendants;
	// descend only those roots, never a retained ignored build directory.
	keep := func(p string, class rules.Class) error {
		return filepath.WalkDir(filepath.Join(res.Root, filepath.FromSlash(p)), func(host string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			rel, err := filepath.Rel(res.Root, host)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if !tracked[rel] && domains.Facts(view, rel, nil).Ignored() {
				roots[rel] = class
				if entry.IsDir() {
					return filepath.SkipDir
				}
			}
			// A tracked file replaces anything formerly beneath it.
			if f, ok := files[rel]; ok && f.Mode != store.ModeDir && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		})
	}
	for p, class := range res.Roots {
		if path.Base(p) == ".git" {
			continue // discarded nested repositories need no object-directory walk
		}
		if class == rules.Hidden {
			class = rules.Local
		}
		if err := keep(p, class); err != nil {
			return err
		}
	}
	for p := range res.Files {
		if !tracked[p] && domains.Facts(view, p, nil).Ignored() {
			roots[p] = rules.Local
		}
	}
	// An ignored descendant can survive a removed untracked directory. Keep
	// its ancestor directories visible without retaining their other files.
	dirs, parents := map[string]bool{}, map[string]bool{}
	for p := range roots {
		dir := path.Dir(p)
		if dir == "." {
			continue
		}
		dirs[dir] = true
		for p := path.Dir(dir); p != "."; p = path.Dir(p) {
			parents[p] = true
		}
	}
	for dir := range dirs {
		if !tracked[dir] && !parents[dir] {
			files[dir] = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
		}
	}
	res.Files, res.Info, res.Roots, res.Repos, res.Domains = files, info, roots, repos, domains
	res.Tree, err = c.Store.Build(files)
	return err
}
