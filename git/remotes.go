package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// Checkout remotes make a repository's checkouts fetchable by name. Unless
// serve --no-git-remotes, the repository gets a remote ovnfs/<checkout> for each
// live checkout, which fetches the checkout's HEAD from its private git
// directory into refs/remotes/ovnfs/<checkout>/HEAD, so `git log
// ovnfs/<checkout>` shows the checkout's work. The remotes live in
// RemotesFile, which the repository's config includes: the folder watcher
// ignores that file, so rewriting it on every create keeps prepared checkouts
// valid, and a private git directory, whose config is a copy, includes a file
// it does not have, so no checkout sees the others' remotes.

// RemotesFile is the config file in a repository's common git directory that
// holds its checkout remotes; ovnfs owns it and rewrites it whole.
const RemotesFile = "ovnfs-remotes"

// remotePrefix starts every checkout remote's name.
const remotePrefix = "ovnfs/"

// remotesHeader opens RemotesFile for whoever finds it.
const remotesHeader = "# Written by ovnfs: one remote per live checkout of this repository.\n" +
	"# ovnfs rewrites this file; edits are lost. git_remotes: false in\n" +
	"# ~/.o2/ovnfs.yml turns it off.\n"

// trackingPrefix is the namespace of checkout remote name's tracking refs.
func trackingPrefix(name string) string { return "refs/remotes/" + remotePrefix + name + "/" }

// SetCheckoutRemotes makes r's checkout remotes exactly remotes, which maps a
// checkout name to its private git directory: it rewrites RemotesFile,
// includes it from r's config, and deletes the tracking refs of the remotes it
// drops, as `git remote remove` would. A name that cannot form a ref name gets
// no remote. Tags are neither fetched nor pruned, whatever fetch.pruneTags
// says, since a checkout's tags are a stale copy of r's.
func (r *Repo) SetCheckoutRemotes(ctx context.Context, remotes map[string]string) error {
	old, err := r.checkoutRemotes()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var b strings.Builder
	b.WriteString(remotesHeader)
	for _, name := range slices.Sorted(maps.Keys(remotes)) {
		head := trackingPrefix(name) + "HEAD"
		if _, err := refs.CheckRefFormat(head, refs.FormatOptions{}); err != nil {
			continue
		}
		b.WriteString("[remote \"" + remotePrefix + name + "\"]\n")
		b.WriteString("\turl = " + configValue(remotes[name]) + "\n")
		b.WriteString("\tfetch = +HEAD:" + head + "\n")
		b.WriteString("\ttagOpt = --no-tags\n\tpruneTags = false\n\tfollowRemoteHEAD = never\n")
	}
	if err := writeIfChanged(filepath.Join(r.CommonDir, RemotesFile), b.String()); err != nil {
		return err
	}
	if err := rewriteConfig(r.CommonDir, func(config string) string { return includeConfig(config, RemotesFile, true) }); err != nil {
		return err
	}
	var gone []string
	for _, name := range old {
		if _, ok := remotes[name]; !ok {
			gone = append(gone, name)
		}
	}
	return r.deleteTracking(ctx, gone)
}

// RemoveCheckoutRemotes undoes SetCheckoutRemotes: it drops the include from
// r's config, RemotesFile, and every checkout remote's tracking refs. A
// repository without RemotesFile is left untouched.
func (r *Repo) RemoveCheckoutRemotes(ctx context.Context) error {
	names, err := r.checkoutRemotes()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := rewriteConfig(r.CommonDir, func(config string) string { return includeConfig(config, RemotesFile, false) }); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(r.CommonDir, RemotesFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return r.deleteTracking(ctx, names)
}

// checkoutRemotes lists the checkout names RemotesFile holds remotes for; a
// missing file is an error wrapping fs.ErrNotExist.
func (r *Repo) checkoutRemotes() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(r.CommonDir, RemotesFile))
	if err != nil {
		return nil, err
	}
	var names []string
	for line := range strings.Lines(string(data)) {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "[remote \""+remotePrefix)
		if name, found := strings.CutSuffix(name, "\"]"); ok && found {
			names = append(names, name)
		}
	}
	return names, nil
}

// deleteTracking deletes the tracking refs of the checkout remotes names. It
// runs git, which wrote them: the refs store cannot delete.
func (r *Repo) deleteTracking(ctx context.Context, names []string) error {
	var doomed []string
	store := r.repo.Refs()
	for _, name := range names {
		found, err := store.List(trackingPrefix(name))
		if err != nil {
			return err
		}
		for _, ref := range found {
			doomed = append(doomed, ref.Name)
		}
	}
	if len(doomed) == 0 {
		return nil
	}
	program, ok := gitProgram()
	if !ok {
		return fmt.Errorf("git is not installed to delete %s", strings.Join(doomed, ", "))
	}
	for _, ref := range doomed {
		if err := r.runGit(ctx, program, nil, "update-ref", "--no-deref", "-d", ref); err != nil {
			return err
		}
	}
	return nil
}

// includeConfig returns git config text with an "[include] path = value"
// entry, appended unless one exists, when want is set, and without any when it
// is not; an [include] section the removal leaves empty goes too. Every other
// line is kept.
func includeConfig(config, value string, want bool) string {
	var out []string
	found, continued := false, false
	inInclude, removed, kept, header := false, false, 0, 0
	closeSection := func() {
		if inInclude && removed && kept == 0 {
			out = slices.Delete(out, header, header+1)
		}
	}
	for line := range strings.Lines(config) {
		if continued {
			continued = continuesLine(line)
			kept++
			out = append(out, line)
			continue
		}
		continued = continuesLine(line)
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			closeSection()
			end := strings.IndexByte(trimmed, ']')
			inInclude = end > 0 && strings.EqualFold(strings.TrimSpace(trimmed[1:end]), "include") && strings.TrimSpace(trimmed[end+1:]) == ""
			removed, kept, header = false, 0, len(out)
			out = append(out, line)
			continue
		}
		if inInclude && strings.EqualFold(configKey(trimmed), "path") && entryValue(trimmed) == value {
			found = true
			if !want {
				removed = true
				continue
			}
		}
		if trimmed != "" {
			kept++
		}
		out = append(out, line)
	}
	closeSection()
	if want && !found {
		if n := len(out); n != 0 && !strings.HasSuffix(out[n-1], "\n") {
			out[n-1] += "\n"
		}
		out = append(out, "[include]\n", "\tpath = "+configValue(value)+"\n")
	}
	return strings.Join(out, "")
}

// entryValue returns the value a "key = value" config line sets, unquoted when
// the whole value is one quoted string.
func entryValue(line string) string {
	_, value, _ := strings.Cut(line, "=")
	value = strings.TrimSpace(value)
	if unquoted, ok := strings.CutPrefix(value, `"`); ok {
		if unquoted, ok = strings.CutSuffix(unquoted, `"`); ok {
			return unquoted
		}
	}
	return value
}

// writeIfChanged replaces file name with data, mode 0644, unless it holds data already.
func writeIfChanged(name, data string) error {
	if old, err := os.ReadFile(name); err == nil && string(old) == data {
		return nil
	}
	return replaceFile(name, data, 0o644)
}
