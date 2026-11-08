package git

import (
	"context"
	"strings"
)

// Conversion is the configuration that decides, together with gitattributes,
// whether git converts content between blobs and the work tree.
type Conversion struct {
	AutoCRLF bool            // core.autocrlf is true or input: unattributed text gets end-of-line conversion
	Filters  map[string]bool // filter drivers with a clean, smudge or process command
}

// Conversion reads the repository's effective core.autocrlf and filter drivers.
func (r *Repo) Conversion(ctx context.Context) (Conversion, error) {
	c := Conversion{Filters: map[string]bool{}}
	cfg, err := r.repo.Config()
	if err != nil {
		return c, err
	}
	for e := range cfg.All() {
		if e.Key == "core.autocrlf" {
			// A valueless key is boolean true; the last assignment wins.
			c.AutoCRLF = !e.HasValue || !isFalse(e.Value)
			continue
		}
		if driver, ok := e.Subsection(); ok && e.Section() == "filter" {
			switch e.Name() {
			case "clean", "smudge", "process":
				c.Filters[driver] = true
			}
		}
	}
	return c, nil
}

// isFalse reports git's spellings of a false boolean.
func isFalse(value string) bool {
	switch strings.ToLower(value) {
	case "false", "no", "off", "0", "":
		return true
	}
	return false
}
