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
	base, err := r.optional(ctx, "merge-base", head, upstream)
	if err != nil {
		return Origin{}, false, err
	}
	if base != "" {
		o.Commit = base
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

// InitBare opens the bare repository at dir, creating it first if needed.
func InitBare(dir string) (*Repo, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &Repo{Top: dir, CommonDir: dir, GitDir: dir}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if _, err := r.text(context.Background(), "init", "--quiet", "--bare"); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// HasOrigin reports whether an earlier FetchOrigin stored commit in this repository.
func (r *Repo) HasOrigin(ctx context.Context, commit string) (bool, error) {
	got, err := r.optional(ctx, "rev-parse", "--verify", "--quiet", "refs/ovnfs/"+commit+"^{commit}")
	return got != "", err
}

// FetchOrigin fetches o's commit, without its history, into this (bare)
// repository under refs/ovnfs/<commit>, which keeps its objects until the ref
// is deleted. It never prompts: a fetch needing credentials this machine lacks
// fails. Only https, http, ssh, git and local transports are allowed.
func (r *Repo) FetchOrigin(ctx context.Context, o Origin) error {
	if err := o.Valid(); err != nil {
		return err
	}
	cmd := r.command(ctx, "-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always", "-c", "protocol.http.allow=always",
		"-c", "protocol.ssh.allow=always", "-c", "protocol.git.allow=always", "-c", "protocol.file.allow=always",
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--depth=1", o.URL, o.Commit+":refs/ovnfs/"+o.Commit)
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	if os.Getenv("GIT_SSH_COMMAND") == "" && os.Getenv("GIT_SSH") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	_, err := run(cmd)
	return err
}
