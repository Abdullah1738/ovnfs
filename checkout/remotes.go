package checkout

import (
	"context"

	"github.com/stencil-hq/ovnfs/git"
)

// syncRemotes gives repo one remote per live checkout of it when
// Config.GitRemotes is set (git.Repo.SetCheckoutRemotes), and removes its
// checkout remotes when it is not, so turning the setting off cleans up. A
// failure is reported as a fault: the checkouts themselves are unaffected. A
// nil repo is ignored.
func (r *Registry) syncRemotes(repo *git.Repo) {
	if repo == nil {
		return
	}
	r.remotesMu.Lock()
	defer r.remotesMu.Unlock()
	// The checkout is created or removed already: finish even if its caller left.
	ctx := context.Background()
	var err error
	if r.config.GitRemotes {
		err = repo.SetCheckoutRemotes(ctx, r.remotesOf(repo.CommonDir))
	} else {
		err = repo.RemoveCheckoutRemotes(ctx)
	}
	if err != nil {
		r.enqueueNotice(baseNotice{fault: "git remotes in " + repo.CommonDir + ": " + err.Error()})
	}
}

// syncAllRemotes runs syncRemotes once for each repository with live checkouts,
// so a changed Config.GitRemotes applies to existing checkouts when the daemon starts.
func (r *Registry) syncAllRemotes() {
	r.mu.RLock()
	repos := make(map[string]*git.Repo)
	for _, h := range r.checkouts {
		if repo := localRepo(h.base); repo != nil {
			repos[repo.CommonDir] = repo
		}
	}
	r.mu.RUnlock()
	for _, repo := range repos {
		r.syncRemotes(repo)
	}
}

// remotesOf maps each live checkout with a private git directory, on a capture
// of the repository whose common git directory is common, to that directory.
func (r *Registry) remotesOf(common string) map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	remotes := make(map[string]string)
	for name, h := range r.checkouts {
		if repo := localRepo(h.base); h.meta.Git && repo != nil && repo.CommonDir == common {
			remotes[name] = r.gitdirPath(h.meta.dir())
		}
	}
	return remotes
}

// localRepo returns the repository at the root of b's capture on this machine:
// nil for a capture of no repository and for one attached from another machine.
func localRepo(b *baseRecord) *git.Repo {
	if b.view.Meta().Remote != nil {
		return nil
	}
	return b.view.Domains().Repo("")
}
