// Package config reads git configuration: the config file syntax, the merged
// view git builds from its system, global, repository, worktree and
// command-line scopes (with include and includeIf directives), typed queries
// (boolean, integer, pathname), and the <section>.<url>.<key> URL matching
// used by http.* and credential.* settings.
//
// It is written from git's documentation (git-config(1), git(1)
// ENVIRONMENT, gitcredentials(7), config/http) and checked against the git
// binary; behaviour the documents leave open was settled by experiment.
//
// A loaded Config is immutable and safe for concurrent use.
package config

import (
	"fmt"
	"iter"
	"math"
	"os"
	"slices"
	"strings"
)

// Scope is the configuration source class an entry came from, as printed by
// `git config --show-scope`.
type Scope uint8

const (
	// ScopeSystem is the system-wide file (GIT_CONFIG_SYSTEM or /etc/gitconfig).
	ScopeSystem Scope = iota + 1
	// ScopeGlobal is the user's files ($XDG_CONFIG_HOME/git/config, ~/.gitconfig, or GIT_CONFIG_GLOBAL).
	ScopeGlobal
	// ScopeLocal is the repository's <common dir>/config.
	ScopeLocal
	// ScopeWorktree is <git dir>/config.worktree, read when extensions.worktreeConfig is on.
	ScopeWorktree
	// ScopeCommand is GIT_CONFIG_COUNT/KEY/VALUE, git -c (GIT_CONFIG_PARAMETERS),
	// and, as with `git config --file`, a single file read by ReadFile or Parse.
	ScopeCommand
)

// String returns git's name for the scope.
func (s Scope) String() string {
	switch s {
	case ScopeSystem:
		return "system"
	case ScopeGlobal:
		return "global"
	case ScopeLocal:
		return "local"
	case ScopeWorktree:
		return "worktree"
	case ScopeCommand:
		return "command"
	}
	return "unknown"
}

// Protected reports whether git trusts settings from this scope for
// security-sensitive options such as safe.directory.
func (s Scope) Protected() bool {
	return s == ScopeSystem || s == ScopeGlobal || s == ScopeCommand
}

// Entry is one variable assignment in the merged configuration.
type Entry struct {
	// Key is the canonical name: section and variable name lower-cased,
	// subsection as written ("remote.Origin.url").
	Key string
	// Value is the assigned value; "" when HasValue is false.
	Value string
	// HasValue is false for a bare "name" line, which booleans read as true.
	HasValue bool
	// File is the file the entry was read from (as opened, after include
	// path resolution); "" for command-line settings.
	File string
	// Line is the 1-based line of the entry in File; 0 for command-line settings.
	Line int
	// Scope is the source class; included files inherit their includer's scope.
	Scope Scope
}

// Section returns the lower-cased section name.
func (e Entry) Section() string {
	s, _, _, _ := splitKey(e.Key)
	return s
}

// Subsection returns the subsection as written, and whether there is one.
func (e Entry) Subsection() (string, bool) {
	_, sub, ok, _ := splitKey(e.Key)
	return sub, ok
}

// Name returns the lower-cased variable name.
func (e Entry) Name() string {
	_, _, _, n := splitKey(e.Key)
	return n
}

// Bool interprets the entry as a boolean (see ParseBool); a bare name is true.
func (e Entry) Bool() (bool, error) {
	if !e.HasValue {
		return true, nil
	}
	v, err := ParseBool(e.Value)
	if err != nil {
		return false, fmt.Errorf("%w for '%s'", err, e.Key)
	}
	return v, nil
}

// Int interprets the entry as a 64-bit integer (see ParseInt).
func (e Entry) Int() (int64, error) {
	v, err := parseInt(e.Value, math.MinInt64, math.MaxInt64)
	if err != nil {
		return 0, fmt.Errorf("bad numeric config value '%s' for '%s' %s: %w", e.Value, e.Key, e.where(), err)
	}
	return v, nil
}

func (e Entry) where() string {
	if e.File == "" {
		return "in command line"
	}
	return "in file " + e.File
}

// Config is a merged configuration: every entry from every source, in the
// order git reads them, so later entries override earlier ones.
type Config struct {
	entries []Entry
	index   map[string][]int // canonical key → positions in entries
	files   []string
	env     func(string) (string, bool)
}

func newConfig(env func(string) (string, bool)) *Config {
	return &Config{index: map[string][]int{}, env: env}
}

func (c *Config) add(e Entry) {
	c.index[e.Key] = append(c.index[e.Key], len(c.entries))
	c.entries = append(c.entries, e)
}

// All yields every entry in precedence order (later wins), as
// `git config --list --show-origin --show-scope` lists them.
func (c *Config) All() iter.Seq[Entry] {
	return slices.Values(c.entries)
}

// Files lists every config file consulted, existing or not, in reading
// order: watching them tells when a reload could give a different result.
func (c *Config) Files() []string {
	return slices.Clone(c.files)
}

// Lookup returns the last entry for key (canonicalised: "Core.IgnoreCase"
// finds core.ignorecase).
func (c *Config) Lookup(key string) (Entry, bool) {
	idx := c.index[lookupKey(key)]
	if len(idx) == 0 {
		return Entry{}, false
	}
	return c.entries[idx[len(idx)-1]], true
}

// LookupAll returns every entry for key in precedence order.
func (c *Config) LookupAll(key string) []Entry {
	idx := c.index[lookupKey(key)]
	out := make([]Entry, len(idx))
	for i, j := range idx {
		out[i] = c.entries[j]
	}
	return out
}

// Get returns the value of key's last entry ("" for a bare name), like
// `git config --get`.
func (c *Config) Get(key string) (string, bool) {
	e, ok := c.Lookup(key)
	return e.Value, ok
}

// GetAll returns every value of key in precedence order, like
// `git config --get-all`.
func (c *Config) GetAll(key string) []string {
	idx := c.index[lookupKey(key)]
	out := make([]string, len(idx))
	for i, j := range idx {
		out[i] = c.entries[j].Value
	}
	return out
}

// Bool returns key's last value as a boolean, like `git config --bool`;
// found is false when key is unset. As with git config, every value of key
// must parse, not just the last.
func (c *Config) Bool(key string) (value, found bool, err error) {
	for _, j := range c.index[lookupKey(key)] {
		if value, err = c.entries[j].Bool(); err != nil {
			return false, true, err
		}
		found = true
	}
	return value, found, nil
}

// Int returns key's last value as an integer, like `git config --int`;
// every value of key must parse.
func (c *Config) Int(key string) (value int64, found bool, err error) {
	for _, j := range c.index[lookupKey(key)] {
		if value, err = c.entries[j].Int(); err != nil {
			return 0, true, err
		}
		found = true
	}
	return value, found, nil
}

// Path returns key's last value as a pathname, like `git config --path`:
// "~/" and "~user/" are expanded, and an entry prefixed ":(optional)" whose
// path does not exist is skipped as if unset. Every value of key must
// expand; "%(prefix)/" values fail with ErrPrefixUnsupported.
func (c *Config) Path(key string) (value string, found bool, err error) {
	for _, j := range c.index[lookupKey(key)] {
		e := c.entries[j]
		if !e.HasValue {
			return "", false, fmt.Errorf("missing value for '%s'", e.Key)
		}
		p, exists, err := expandPath(e.Value, c.env)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", e.Key, err)
		}
		if exists {
			value, found = p, true
		}
	}
	return value, found, nil
}

// Subsections lists the distinct subsections of section (case-insensitive)
// in order of first appearance: the remotes for "remote", branches for
// "branch", base URLs for "url".
func (c *Config) Subsections(section string) []string {
	section = strings.ToLower(section)
	var out []string
	seen := map[string]bool{}
	for _, e := range c.entries {
		s, sub, ok, _ := splitKey(e.Key)
		if ok && s == section && !seen[sub] {
			seen[sub] = true
			out = append(out, sub)
		}
	}
	return out
}

// ReadFile parses one config file without following includes, like
// `git config --file <path> --list`. Its entries have ScopeCommand, as git
// reports for --file.
func ReadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// Parse parses config text; name labels errors and entries' File.
func Parse(data []byte, name string) (*Config, error) {
	raw, err := parse(data, name)
	if err != nil {
		return nil, err
	}
	c := newConfig(os.LookupEnv)
	c.files = []string{name}
	for _, r := range raw {
		c.add(Entry{Key: r.key, Value: r.value, HasValue: r.hasValue, File: name, Line: r.line, Scope: ScopeCommand})
	}
	return c, nil
}
