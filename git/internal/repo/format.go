package repo

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// settings is what a git directory's own configuration says about the
// repository's layout, read as git does before trusting the repository.
type settings struct {
	format         object.Format
	backend        refs.Backend
	worktreeConfig bool
	bare, bareSet  bool
	worktree       string
	worktreeSet    bool
}

// Extensions git 2.56 honours in a version-0 repository (for historical
// reasons) and those it honours only in version 1. Others are ignored in
// version 0 and refused in version 1. compatObjectFormat is known to git
// but refused here: we cannot maintain the compatibility object map.
var (
	v0Extensions = map[string]bool{"noop": true, "preciousobjects": true, "partialclone": true, "worktreeconfig": true}
	v1Extensions = map[string]bool{
		"noop-v1": true, "objectformat": true, "refstorage": true, "relativeworktrees": true,
		"submodulepathconfig": true, "compatobjectformat": true,
	}
)

// readSettings checks the repository format in <commonDir>/config and reads
// core.bare and core.worktree. Only that file is read (no includes, no other
// scopes), plus <gitDir>/config.worktree when extensions.worktreeConfig is
// set. A linked worktree ignores core.bare and core.worktree from the common
// config unless extensions.worktreeConfig is set.
func readSettings(gitDir, commonDir string) (settings, error) {
	s := settings{format: object.SHA1, backend: refs.BackendFiles}
	common, err := readConfigFile(filepath.Join(commonDir, "config"))
	if err != nil {
		return s, err
	}
	if err := s.checkFormat(common); err != nil {
		return s, fmt.Errorf("repo: %s: %w", commonDir, err)
	}
	if gitDir == commonDir || s.worktreeConfig {
		if err := s.readCore(common); err != nil {
			return s, err
		}
	}
	if s.worktreeConfig {
		wt, err := readConfigFile(filepath.Join(gitDir, "config.worktree"))
		if err != nil {
			return s, err
		}
		if err := s.readCore(wt); err != nil {
			return s, err
		}
	}
	return s, nil
}

// readConfigFile parses one config file; a missing file is empty.
func readConfigFile(name string) (*config.Config, error) {
	cfg, err := config.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return config.Parse(nil, name)
	}
	return cfg, err
}

// checkFormat applies git's repository format rules
// (technical/repository-version): a version above 1 is refused; in version
// 1 every extension must be known; in version 0 unknown extensions are
// ignored but version-1-only ones are refused. Invalid objectFormat and
// refStorage values are refused in any version.
func (s *settings) checkFormat(cfg *config.Config) error {
	version, _, err := cfg.Int("core.repositoryformatversion")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	if version > 1 {
		return fmt.Errorf("%w: expected git repo version <= 1, found %d", ErrUnsupportedFormat, version)
	}
	exts := map[string]config.Entry{}
	for e := range cfg.All() {
		if _, sub := e.Subsection(); e.Section() == "extensions" && !sub {
			exts[e.Name()] = e
		}
	}
	for _, name := range slices.Sorted(maps.Keys(exts)) {
		e := exts[name]
		switch name {
		case "objectformat":
			f, err := object.ParseFormat(e.Value)
			if err != nil || !e.HasValue {
				return fmt.Errorf("%w: invalid value for 'extensions.objectformat': '%s'", ErrUnsupportedFormat, e.Value)
			}
			s.format = f
		case "refstorage":
			b := refs.Backend(e.Value)
			if b != refs.BackendFiles && b != refs.BackendReftable {
				return fmt.Errorf("%w: invalid value for 'extensions.refstorage': '%s'", ErrUnsupportedFormat, e.Value)
			}
			s.backend = b
		}
		known := v0Extensions[name] || v1Extensions[name]
		switch {
		case version == 1 && !known:
			return fmt.Errorf("%w: unknown repository extension found: %s", ErrUnsupportedFormat, name)
		case version == 1 && name == "compatobjectformat":
			return fmt.Errorf("%w: extensions.compatObjectFormat is not supported", ErrUnsupportedFormat)
		case version != 1 && v1Extensions[name]:
			return fmt.Errorf("%w: repo version is %d, but v1-only extension found: %s", ErrUnsupportedFormat, version, name)
		}
	}
	if e, ok := exts["worktreeconfig"]; ok {
		v, err := e.Bool()
		if err != nil {
			return err
		}
		s.worktreeConfig = v
	}
	return nil
}

// readCore takes core.bare and core.worktree from cfg when it sets them.
func (s *settings) readCore(cfg *config.Config) error {
	if v, ok, err := cfg.Bool("core.bare"); err != nil {
		return err
	} else if ok {
		s.bare, s.bareSet = v, true
	}
	if v, ok := cfg.Get("core.worktree"); ok {
		s.worktree, s.worktreeSet = v, true
	}
	return nil
}
