package repo

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
)

// isOwned reports whether path belongs to the current user (see
// ownedByCurrentUser); tests replace it to simulate foreign ownership.
var isOwned = ownedByCurrentUser

// checkOwnership applies safe.directory (config/safe.adoc): when any of
// paths is owned by someone else, the repository is refused unless dir is
// listed in safe.directory in the protected configuration (system, global,
// command line). gitDir and workTree only feed includeIf conditions.
func checkOwnership(paths []string, dir, gitDir, workTree string) error {
	foreign := ""
	for _, p := range paths {
		ok, err := isOwned(p)
		if err != nil {
			return err
		}
		if !ok {
			foreign = p
			break
		}
	}
	if foreign == "" {
		return nil
	}
	cfg, err := config.LoadProtected(config.Options{GitDir: gitDir, WorkTree: workTree})
	if err != nil {
		return err
	}
	if safeDirectory(cfg.GetAll("safe.directory"), dir) {
		return nil
	}
	return fmt.Errorf("repo: %s: %w: %s is owned by someone else (add it to safe.directory)", dir, ErrUnsafe, foreign)
}

// safeDirectory evaluates safe.directory values in order: "" empties the
// list, "*" allows everything, "<path>/*" allows dir below path, and any
// other value allows dir equal to it. Values have "~/" and "~user/"
// expanded and are compared both as written (cleaned) and resolved to
// physical paths; "%(prefix)/" values are skipped (git's install prefix is
// unknown).
func safeDirectory(values []string, dir string) bool {
	safe := false
	dir = filepath.ToSlash(dir)
	for _, v := range values {
		switch v {
		case "":
			safe = false
			continue
		case "*":
			safe = true
			continue
		}
		if strings.HasPrefix(v, "%(prefix)/") {
			continue
		}
		pattern, prefix := strings.CutSuffix(v, "/*")
		p, ok := expandHome(pattern)
		if !ok {
			continue
		}
		for _, cand := range []string{filepath.Clean(p), physical(p)} {
			cand = filepath.ToSlash(cand)
			if prefix && within(dir, cand) && dir != cand || !prefix && dir == cand {
				safe = true
			}
		}
	}
	return safe
}

// expandHome expands a leading "~/" or "~user/" as git interpolates paths.
func expandHome(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, "~")
	if !ok {
		return p, true
	}
	name, tail, _ := strings.Cut(rest, "/")
	var home string
	if name == "" {
		h, ok := os.LookupEnv("HOME")
		if !ok && runtime.GOOS == "windows" {
			h, ok = os.LookupEnv("USERPROFILE")
		}
		if !ok {
			return "", false
		}
		home = h
	} else {
		u, err := user.Lookup(name)
		if err != nil {
			return "", false
		}
		home = u.HomeDir
	}
	return filepath.Join(home, tail), true
}
