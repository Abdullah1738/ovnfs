package rules

import (
	"path"
	"strings"
)

// secretFiles are exact credential file names.
var secretFiles = map[string]bool{
	".env":             true,
	".netrc":           true,
	".npmrc":           true,
	".pypirc":          true,
	".git-credentials": true,
	"credentials":      true,
	"credentials.json": true,
	"credentials.toml": true,
	"id_rsa":           true,
	"id_dsa":           true,
	"id_ecdsa":         true,
	"id_ed25519":       true,
}

// secretDirs are directory names that hold credentials.
var secretDirs = map[string]bool{
	".ssh":    true,
	".gnupg":  true,
	".aws":    true,
	".kube":   true,
	".docker": true,
}

// secretExts are file extensions of key and certificate bundles.
var secretExts = []string{".pem", ".key", ".p12", ".pfx"}

// envTemplates are `.env.*` suffixes that mark committed examples, not secrets.
var envTemplates = []string{".example", ".sample", ".template", ".dist"}

// IsSecret reports whether a path's name is a known credential file or
// directory (used by the `secret` predicate and by plan warnings).
func IsSecret(p string, dir bool) bool {
	name := path.Base(p)
	if dir {
		return secretDirs[name]
	}
	return isSecretFile(name)
}

// isSecretFile reports whether name is a credential file name.
func isSecretFile(name string) bool {
	if secretFiles[name] {
		return true
	}
	if strings.HasPrefix(name, ".env.") {
		for _, t := range envTemplates {
			if strings.HasSuffix(name, t) {
				return false
			}
		}
		return true
	}
	for _, ext := range secretExts {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// secretNode is the `secret` keyword; it consults Dir only when the name could be a secret.
type secretNode struct{}

// eval checks the name first so most paths never touch the filesystem.
func (secretNode) eval(p string, f Facts) bool {
	name := path.Base(p)
	if isSecretFile(name) {
		return !f.Dir()
	}
	if secretDirs[name] {
		return f.Dir()
	}
	return false
}
