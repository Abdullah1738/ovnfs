package checkout

import (
	"context"
	"path"
	"strings"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
)

func resolveTakes(ctx context.Context, root string, specs []string) ([]base.Take, error) {
	var takes []base.Take
	var repo *git.Repo
	revisions := map[string]*git.Revision{}
	for _, spec := range specs {
		src, p, ok := strings.Cut(spec, ":")
		if !ok || strings.ContainsRune(p, '\x00') || (p != "" && (path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../"))) {
			return nil, failure("invalid", "invalid --take %q: expected SRC:PATH with a normalized repo-relative slash path", spec)
		}
		take := base.Take{Source: src, Path: p}
		if src != "." {
			if repo == nil {
				var err error
				repo, err = git.Open(root)
				if err != nil {
					return nil, failure("invalid", "--take source %q needs a git repository: %s", src, root)
				}
			}
			if src != "" {
				v := revisions[src]
				if v == nil {
					var err error
					v, err = repo.ResolveRevision(ctx, src)
					if err != nil {
						return nil, failure("invalid", "cannot resolve --take revision %q: %v", src, err)
					}
					revisions[src] = v
				}
				take.Revision = v
			}
		}
		takes = append(takes, take)
	}
	return takes, nil
}
