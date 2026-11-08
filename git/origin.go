package git

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/fetch"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

// Origin names a commit on a repository's upstream remote: where a machine
// without the source folder can fetch that repository's committed content.
type Origin struct {
	URL    string `json:"url"`
	Commit string `json:"commit"`
}

// Valid rejects origins that git could misread: an empty or option-like URL,
// control characters, or a commit that is not a full hex object name.
func (o Origin) Valid() error {
	if o.URL == "" || strings.HasPrefix(o.URL, "-") || strings.ContainsFunc(o.URL, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return fmt.Errorf("invalid origin URL %q", o.URL)
	}
	if len(o.Commit) != 40 && len(o.Commit) != 64 {
		return fmt.Errorf("invalid origin commit %q", o.Commit)
	}
	if _, err := hex.DecodeString(o.Commit); err != nil || strings.ToLower(o.Commit) != o.Commit {
		return fmt.Errorf("invalid origin commit %q", o.Commit)
	}
	return nil
}

// Origin returns where another machine can fetch this repository's committed
// content: its upstream's URL, without credentials, and a commit the upstream
// has. That is head when the upstream contains it, else head's merge base with
// the upstream, else the upstream's tip. ok is false without an upstream.
func (r *Repo) Origin(ctx context.Context, head string) (o Origin, ok bool, err error) {
	ref, upstream, err := r.Upstream(ctx)
	if err != nil || upstream == "" {
		return Origin{}, false, err
	}
	remote, err := r.Remote(ctx, ref)
	if err != nil || remote == "" {
		return Origin{}, false, err
	}
	o = Origin{URL: redact(remote), Commit: upstream}
	if head == "" {
		return o, true, nil
	}
	pushed, err := r.Contains(ctx, upstream, head)
	if err != nil {
		return Origin{}, false, err
	}
	if pushed {
		o.Commit = head
		return o, true, nil
	}
	err = r.session(func(s *rev.Source) error {
		a, b, err := commitIDs(s, head, upstream)
		if err != nil {
			return err
		}
		base, found, err := rev.MergeBase(s, a, b)
		if found {
			o.Commit = base.String()
		}
		return err
	})
	if err != nil {
		return Origin{}, false, err
	}
	return o, true, nil
}

// redact drops user information (tokens, passwords) from a URL-shaped remote;
// scp-like and path remotes have none to drop.
func redact(remote string) string {
	if !strings.Contains(remote, "://") {
		return remote
	}
	u, err := url.Parse(remote)
	if err != nil || u.User == nil {
		return remote
	}
	if _, hasPassword := u.User.Password(); !hasPassword && u.Scheme == "ssh" {
		return remote // ssh://git@host/... names an account, not a secret
	}
	u.User = nil
	return u.String()
}

// InitBare opens the bare repository at dir, creating it first as `git init
// --bare` would if needed.
func InitBare(dir string) (*Repo, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var h *repo.Repository
	_, err := os.Stat(filepath.Join(dir, "HEAD"))
	if errors.Is(err, os.ErrNotExist) {
		h, err = repo.InitBare(dir)
	} else {
		h, err = repo.Open(dir)
	}
	if err != nil {
		return nil, err
	}
	return newRepo(h), nil
}

// HasOrigin reports whether an earlier FetchOrigin stored commit in this repository.
func (r *Repo) HasOrigin(ctx context.Context, c string) (have bool, err error) {
	err = r.session(func(s *rev.Source) error {
		got, err := resolveCommit(s, "refs/ovnfs/"+c)
		have = got != ""
		return err
	})
	return have, err
}

// FetchOrigin fetches o's commit, without its history, into this (bare)
// repository under refs/ovnfs/<commit>, which keeps its objects until the ref
// is deleted. It runs `git fetch` when git is installed, else fetches in
// process. It never prompts: a fetch needing credentials this machine lacks
// fails. Only https, http, ssh, git and local transports are allowed.
func (r *Repo) FetchOrigin(ctx context.Context, o Origin) error {
	if err := o.Valid(); err != nil {
		return err
	}
	if program, ok := gitProgram(); ok {
		args := []string{"-c", "protocol.allow=never"}
		for _, transport := range originTransports {
			args = append(args, "-c", "protocol."+transport+".allow=always")
		}
		args = append(args, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--depth=1", o.URL, o.Commit+":refs/ovnfs/"+o.Commit)
		env := []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"}
		if command, ok := batchSSH(); ok {
			env = append(env, "GIT_SSH_COMMAND="+command)
		}
		return r.runGit(ctx, program, env, args...)
	}
	want, err := object.ParseIDFormat(r.repo.Format, o.Commit)
	if err != nil {
		return fmt.Errorf("origin commit %s: %w", o.Commit, err)
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return err
	}
	shallow, err := fetch.ReadShallow(r.CommonDir)
	if err != nil {
		return err
	}
	// Commits fetched before let the server leave out what they share.
	fetched, err := r.repo.Refs().List("refs/ovnfs/")
	if err != nil {
		return err
	}
	var haves []object.ID
	for _, ref := range fetched {
		if !ref.Symbolic() {
			haves = append(haves, ref.ID)
		}
	}
	db, err := r.repo.OpenDB()
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := fetch.Fetch(ctx, fetch.Options{
		URL:           o.URL,
		Wants:         []object.ID{want},
		Depth:         1,
		Shallow:       shallow,
		Haves:         haves,
		Allowed:       originTransports,
		Config:        cfg,
		Env:           originEnv,
		Target:        db,
		GitDir:        r.CommonDir,
		OpenLocal:     openLocal,
		MaxObjectSize: originMaxObjectSize,
	})
	if err != nil {
		return err
	}
	if err := fetch.UpdateShallow(r.CommonDir, res.Shallow, res.Unshallow); err != nil {
		return err
	}
	reflog, err := r.reflog("fetch " + o.URL + " " + o.Commit + ":refs/ovnfs/" + o.Commit + ": storing ref")
	if err != nil {
		return err
	}
	return r.repo.Refs().Update("refs/ovnfs/"+o.Commit, want, refs.UpdateOptions{AnyOld: true, Reflog: reflog})
}

// originTransports are the transports an origin fetch may use.
var originTransports = []string{"https", "http", "ssh", "git", "file"}

// originMaxObjectSize bounds every object an in-process origin fetch accepts.
// The URL and the server come from a peer and pack decompression runs in this
// process, so a pack declaring a bigger object is refused before it is
// inflated; files that large (beyond any forge's limit) are read over the link
// instead.
const originMaxObjectSize = 512 << 20

// batchSSH returns the ssh command origin fetches use so that ssh never
// prompts, unless the user chose one through GIT_SSH_COMMAND or GIT_SSH.
func batchSSH() (string, bool) {
	if os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != "" {
		return "", false
	}
	return "ssh -o BatchMode=yes", true
}

// originEnv is the environment in-process origin fetches run in: ssh in batch
// mode (prompts on terminals and through credential managers are disabled by
// package fetch).
func originEnv(key string) (string, bool) {
	if key == "GIT_SSH_COMMAND" {
		if command, ok := batchSSH(); ok {
			return command, true
		}
	}
	return os.LookupEnv(key)
}

// openLocal opens the repository a local fetch URL names, trying path/.git,
// path, path.git/.git and path.git as git's upload-pack does, and returns its
// raw object database and shallow boundary.
func openLocal(path string) (*object.DB, []object.ID, error) {
	var h *repo.Repository
	var err error
	for _, candidate := range []string{filepath.Join(path, ".git"), path, path + ".git/.git", path + ".git"} {
		if h, err = repo.Open(candidate); err == nil {
			break
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s: not a git repository: %w", path, err)
	}
	shallow, err := fetch.ReadShallow(h.CommonDir)
	if err != nil {
		return nil, nil, err
	}
	db, err := h.OpenDB()
	if err != nil {
		return nil, nil, err
	}
	return db, shallow, nil
}
