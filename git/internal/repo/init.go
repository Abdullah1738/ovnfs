package repo

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// The files git's default template directory puts in a new repository,
// byte for byte (sample hooks omitted).
const (
	templateDescription = "Unnamed repository; edit this file 'description' to name the repository.\n"
	templateExclude     = "# git ls-files --others --exclude-from=.git/info/exclude\n" +
		"# Lines that start with '#' are comments.\n" +
		"# For a project mostly in C, the following would be a good set of\n" +
		"# exclude patterns (uncomment them if you want to use them):\n" +
		"# *.[oa]\n" +
		"# *~\n"
)

// InitBare creates a bare repository at dir as `git init --bare` does, or
// opens it when dir already has a HEAD. It writes HEAD (the branch from
// init.defaultBranch, default "master"), a config whose core settings come
// from the same filesystem probes git makes (core.filemode, core.symlinks,
// core.ignorecase, core.precomposeunicode on macOS), objects/{info,pack},
// refs/{heads,tags}, description and info/exclude. The object format follows
// GIT_DEFAULT_HASH or init.defaultObjectFormat. The ref storage format is
// always files: the repository is ovnfs's private mirror, whose ref format
// nobody else sees, so the default ref format settings
// (GIT_DEFAULT_REF_STORAGE_FORMAT, GIT_DEFAULT_REF_FORMAT,
// init.defaultRefStorageFormat, init.defaultRefFormat) are ignored rather
// than letting a reftable preference make the mirror fail. init.templateDir
// and sample hooks are ignored too.
func InitBare(dir string) (*Repository, error) {
	if _, err := os.Lstat(filepath.Join(dir, "HEAD")); err == nil {
		return Open(dir)
	}
	cfg, err := config.Load(config.Options{})
	if err != nil {
		return nil, err
	}
	branch := "master"
	if v, ok := cfg.Get("init.defaultBranch"); ok && v != "" {
		branch = v
	}
	if err := refs.CheckBranchName(branch); err != nil {
		return nil, fmt.Errorf("repo: invalid initial branch name %q: %w", branch, err)
	}
	format, err := initFormat(cfg)
	if err != nil {
		return nil, err
	}

	for _, sub := range []string{"objects/info", "objects/pack", "refs/heads", "refs/tags", "info"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(sub)), 0o777); err != nil {
			return nil, err
		}
	}
	files := []struct{ name, content string }{
		{"description", templateDescription},
		{"info/exclude", templateExclude},
		{"HEAD", "ref: refs/heads/" + branch + "\n"},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f.name)), []byte(f.content), 0o666); err != nil {
			return nil, err
		}
	}
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, []byte(initConfig(format, nil)), 0o666); err != nil {
		return nil, err
	}
	probes, err := probeFilesystem(dir, configPath)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(configPath, []byte(initConfig(format, probes)), 0o666); err != nil {
		return nil, err
	}
	return Open(dir)
}

// initFormat picks the object format: GIT_DEFAULT_HASH, else
// init.defaultObjectFormat, else SHA-1.
func initFormat(cfg *config.Config) (object.Format, error) {
	name, ok := os.LookupEnv("GIT_DEFAULT_HASH")
	if !ok {
		name, _ = cfg.Get("init.defaultObjectFormat")
	}
	f, err := object.ParseFormat(name)
	if err != nil {
		return 0, fmt.Errorf("repo: %w", err)
	}
	return f, nil
}

// coreProbes are the core settings git derives from the filesystem.
type coreProbes struct {
	filemode, symlinks, ignorecase bool
	// precompose is set on macOS only.
	precompose *bool
}

// initConfig renders the config git init --bare writes; nil probes give the
// provisional file the probes run against.
func initConfig(format object.Format, p *coreProbes) string {
	var b strings.Builder
	version := 0
	if format != object.SHA1 {
		version = 1
		fmt.Fprintf(&b, "[extensions]\n\tobjectformat = %s\n", format)
	}
	fmt.Fprintf(&b, "[core]\n\trepositoryformatversion = %d\n", version)
	if p == nil {
		return b.String()
	}
	fmt.Fprintf(&b, "\tfilemode = %t\n\tbare = true\n", p.filemode)
	if !p.symlinks {
		b.WriteString("\tsymlinks = false\n")
	}
	if p.ignorecase {
		b.WriteString("\tignorecase = true\n")
	}
	if p.precompose != nil {
		fmt.Fprintf(&b, "\tprecomposeunicode = %t\n", *p.precompose)
	}
	return b.String()
}

// probeFilesystem tests, inside the new git directory, whether the
// filesystem keeps the executable bit (toggling it on file), supports
// symlinks, folds case, and (macOS) finds a precomposed name by its
// decomposed spelling.
func probeFilesystem(dir, file string) (*coreProbes, error) {
	p := &coreProbes{}
	before, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(file, before.Mode().Perm()^0o100); err == nil {
		if after, err := os.Stat(file); err == nil {
			p.filemode = after.Mode().Perm()&0o100 != before.Mode().Perm()&0o100
		}
		if err := os.Chmod(file, before.Mode().Perm()); err != nil {
			return nil, err
		}
	}

	link := filepath.Join(dir, "tXXXXXX"+randomSuffix())
	if err := os.Symlink("testing", link); err == nil {
		p.symlinks = true
		if err := os.Remove(link); err != nil {
			return nil, err
		}
	}

	folded := filepath.Join(filepath.Dir(file), swapCase(filepath.Base(file)))
	if fi, err := os.Stat(folded); err == nil && os.SameFile(fi, before) {
		p.ignorecase = true
	}

	if runtime.GOOS == "darwin" {
		const precomposed, decomposed = "\u00c4", "A\u0308"
		suffix := randomSuffix()
		probe := filepath.Join(dir, precomposed+suffix)
		f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
		_, err = os.Lstat(filepath.Join(dir, decomposed+suffix))
		v := err == nil
		p.precompose = &v
		if err := os.Remove(probe); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// swapCase inverts the case of ASCII letters.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 'a' + 'A'
		case c >= 'A' && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// randomSuffix returns a short random name component for probe files.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
