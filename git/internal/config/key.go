package config

import (
	"errors"
	"strings"
)

// CanonicalKey validates a variable name the way git validates keys given on
// the command line (git -c, GIT_CONFIG_KEY_<n>, git config <key>) and returns
// its canonical spelling: section and variable name lower-cased, subsection
// kept as written ("Remote.Origin.URL" → "remote.Origin.url").
func CanonicalKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("empty config key")
	}
	first := strings.IndexByte(key, '.')
	last := strings.LastIndexByte(key, '.')
	if first <= 0 {
		return "", errors.New("key does not contain a section: " + key)
	}
	if last == len(key)-1 {
		return "", errors.New("key does not contain variable name: " + key)
	}
	if strings.IndexByte(key, '\n') >= 0 {
		return "", errors.New("invalid key (newline): " + key)
	}
	section, name := key[:first], key[last+1:]
	for i := range len(section) {
		if c := section[i]; !isAlnum(c) && c != '-' {
			return "", errors.New("invalid key: " + key)
		}
	}
	if !isAlpha(name[0]) {
		return "", errors.New("invalid key: " + key)
	}
	for i := range len(name) {
		if c := name[i]; !isAlnum(c) && c != '-' {
			return "", errors.New("invalid key: " + key)
		}
	}
	return strings.ToLower(section) + key[first:last+1] + strings.ToLower(name), nil
}

// lookupKey canonicalises a key for a query. Unlike CanonicalKey it accepts
// any section spelling, since file sections may contain dots ([a.b "c"]).
func lookupKey(key string) string {
	first := strings.IndexByte(key, '.')
	last := strings.LastIndexByte(key, '.')
	if first < 0 {
		return strings.ToLower(key)
	}
	return strings.ToLower(key[:first]) + key[first:last+1] + strings.ToLower(key[last+1:])
}

// splitKey splits a canonical key at its first and last dots.
func splitKey(key string) (section, subsection string, hasSub bool, name string) {
	first := strings.IndexByte(key, '.')
	if first < 0 {
		return "", "", false, key
	}
	last := strings.LastIndexByte(key, '.')
	if first == last {
		return key[:first], "", false, key[last+1:]
	}
	return key[:first], key[first+1 : last], true, key[last+1:]
}
