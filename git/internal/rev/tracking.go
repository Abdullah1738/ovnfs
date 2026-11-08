package rev

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// Refspec is one parsed fetch or push refspec: "[+]<src>[:<dst>]", or a
// negative "^<src>" that excludes the refs <src> matches.
type Refspec struct {
	Src, Dst string
	Force    bool
	Negative bool
	// Pattern reports a glob refspec: Src (and Dst, if any) contain one '*'.
	Pattern bool
}

// ParseRefspec parses a refspec as git-fetch and git-push document it. A
// glob needs exactly one '*' on each side that has a name; negative
// refspecs take no destination and no '+'.
func ParseRefspec(s string) (Refspec, error) {
	var rs Refspec
	switch {
	case strings.HasPrefix(s, "^"):
		rs.Negative = true
		s = s[1:]
	case strings.HasPrefix(s, "+"):
		rs.Force = true
		s = s[1:]
	}
	src, dst, hasDst := strings.Cut(s, ":")
	if rs.Negative && (hasDst || src == "") {
		return Refspec{}, fmt.Errorf("invalid negative refspec %q", s)
	}
	rs.Src, rs.Dst = src, dst
	srcStars, dstStars := strings.Count(src, "*"), strings.Count(dst, "*")
	switch {
	case srcStars > 1 || dstStars > 1:
		return Refspec{}, fmt.Errorf("invalid refspec %q: more than one '*'", s)
	case srcStars == 1 && hasDst && dst != "" && dstStars != 1,
		srcStars == 0 && dstStars == 1:
		return Refspec{}, fmt.Errorf("invalid refspec %q: '*' on one side only", s)
	}
	rs.Pattern = srcStars == 1
	for _, name := range []string{src, dst} {
		if name == "" || name == "@" || isHex(name) && (len(name) == 40 || len(name) == 64) {
			continue
		}
		if _, err := refs.CheckRefFormat(name, refs.FormatOptions{AllowOneLevel: true, RefspecPattern: true}); err != nil {
			return Refspec{}, fmt.Errorf("invalid refspec %q: %w", s, err)
		}
	}
	return rs, nil
}

// matchGlob matches name against a pattern with at most one '*' and
// returns what the '*' stood for.
func matchGlob(pattern, name string) (string, bool) {
	before, after, glob := strings.Cut(pattern, "*")
	if !glob {
		return "", pattern == name
	}
	if len(name) < len(before)+len(after) || !strings.HasPrefix(name, before) || !strings.HasSuffix(name, after) {
		return "", false
	}
	return name[len(before) : len(name)-len(after)], true
}

// MapRef maps name through refspecs from source to destination side, as git
// maps a remote branch to its remote-tracking ref: the first refspec whose
// source matches wins. Negative refspecs only narrow what a fetch
// transfers; git ignores them here (observed with @{upstream}).
func MapRef(specs []Refspec, name string) (string, bool) {
	for _, rs := range specs {
		if rs.Negative || rs.Dst == "" {
			continue
		}
		star, ok := matchGlob(rs.Src, name)
		if !ok {
			continue
		}
		if rs.Pattern {
			return strings.Replace(rs.Dst, "*", star, 1), true
		}
		return rs.Dst, true
	}
	return "", false
}

// Tracking answers which ref a branch builds on (@{upstream}) and where it
// would be pushed (@{push}), from branch.*, remote.* and push.* settings.
type Tracking struct {
	Config *config.Config
}

func (t *Tracking) get(key string) string {
	if t.Config == nil {
		return ""
	}
	v, _ := t.Config.Get(key)
	return v
}

func (t *Tracking) refspecs(remote, kind string) ([]Refspec, error) {
	if t.Config == nil {
		return nil, nil
	}
	var specs []Refspec
	for _, v := range t.Config.GetAll("remote." + remote + "." + kind) {
		rs, err := ParseRefspec(v)
		if err != nil {
			return nil, err
		}
		specs = append(specs, rs)
	}
	return specs, nil
}

// remoteExists reports whether a remote is configured (any remote.<name>.*).
func (t *Tracking) remoteExists(name string) bool {
	return t.Config != nil && slices.Contains(t.Config.Subsections("remote"), name)
}

// Upstream returns the full name of the ref branch builds on: the
// remote-tracking ref of branch.<branch>.merge under branch.<branch>.remote's
// fetch refspecs, or the local branch itself for the remote ".".
func (t *Tracking) Upstream(branch string) (string, error) {
	remote := t.get("branch." + branch + ".remote")
	merge := t.get("branch." + branch + ".merge")
	if t.Config != nil {
		// git uses the first merge value for @{upstream}.
		if all := t.Config.GetAll("branch." + branch + ".merge"); len(all) > 0 {
			merge = all[0]
		}
	}
	if remote == "" || merge == "" {
		return "", fmt.Errorf("%w for branch '%s'", ErrNoUpstream, branch)
	}
	if remote == "." {
		return merge, nil
	}
	specs, err := t.refspecs(remote, "fetch")
	if err != nil {
		return "", err
	}
	tracking, ok := MapRef(specs, merge)
	if !ok {
		return "", fmt.Errorf("%w: upstream branch '%s' not stored as a remote-tracking branch", ErrNoUpstream, merge)
	}
	return tracking, nil
}

// PushRemote returns the remote branch would push to:
// branch.<branch>.pushRemote, else remote.pushDefault, else
// branch.<branch>.remote, else "origin". A URL naming exactly one
// configured remote's push URL is replaced by that remote.
func (t *Tracking) PushRemote(branch string) string {
	remote := t.get("branch." + branch + ".pushremote")
	if remote == "" {
		remote = t.get("remote.pushdefault")
	}
	if remote == "" {
		remote = t.get("branch." + branch + ".remote")
	}
	if remote == "" {
		remote = "origin"
	}
	if remote != "." && !t.remoteExists(remote) && t.Config != nil {
		var match []string
		for _, name := range t.Config.Subsections("remote") {
			urls := t.Config.GetAll("remote." + name + ".pushurl")
			if len(urls) == 0 {
				urls = t.Config.GetAll("remote." + name + ".url")
			}
			if slices.Contains(urls, remote) {
				match = append(match, name)
			}
		}
		if len(match) == 1 {
			remote = match[0]
		}
	}
	return remote
}

// Push returns the full name of the ref that tracks where `git push` would
// update when run on branch: remote.<r>.push refspecs if configured, else
// push.default (simple by default), mapped through the push remote's fetch
// refspecs.
func (t *Tracking) Push(branch string) (string, error) {
	remote := t.PushRemote(branch)
	local := "refs/heads/" + branch
	if remote != "." && !t.remoteExists(remote) {
		return "", fmt.Errorf("%w: branch '%s' has no remote for pushing", ErrNoUpstream, branch)
	}
	push, err := t.refspecs(remote, "push")
	if err != nil {
		return "", err
	}
	var dst string
	if len(push) > 0 {
		var ok bool
		if dst, ok = MapRef(push, local); !ok {
			return "", fmt.Errorf("%w: push refspecs for '%s' do not include '%s'", ErrNoUpstream, remote, local)
		}
	} else {
		// Observed: for @{push}, "upstream" is simply @{upstream} and
		// "simple" is @{upstream} provided it has the branch's own name,
		// whatever remote pushes go to.
		switch mode := strings.ToLower(t.get("push.default")); mode {
		case "nothing":
			return "", fmt.Errorf("%w: push.default is nothing", ErrNoUpstream)
		case "matching", "current":
			dst = local
		case "upstream", "tracking":
			return t.Upstream(branch)
		case "simple", "":
			up, err := t.Upstream(branch)
			if err != nil {
				return "", err
			}
			if merge, _ := t.mergeRef(branch); merge != local {
				return "", fmt.Errorf("%w: cannot resolve 'simple' push to a single destination", ErrNoUpstream)
			}
			return up, nil
		default:
			return "", fmt.Errorf("%w: unknown push.default %q", ErrNoUpstream, mode)
		}
	}
	if remote == "." {
		return dst, nil
	}
	fetch, err := t.refspecs(remote, "fetch")
	if err != nil {
		return "", err
	}
	tracking, ok := MapRef(fetch, dst)
	if !ok {
		return "", fmt.Errorf("%w: push destination '%s' on remote '%s' has no local tracking branch", ErrNoUpstream, dst, remote)
	}
	return tracking, nil
}

// mergeRef returns the first branch.<branch>.merge value.
func (t *Tracking) mergeRef(branch string) (string, error) {
	if t.Config != nil {
		if all := t.Config.GetAll("branch." + branch + ".merge"); len(all) > 0 && all[0] != "" {
			return all[0], nil
		}
	}
	return "", fmt.Errorf("%w for branch '%s'", ErrNoUpstream, branch)
}
