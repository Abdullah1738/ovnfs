package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// commandEntries returns the command scope: GIT_CONFIG_COUNT pairs, then the
// `git -c` settings a parent git exports in GIT_CONFIG_PARAMETERS, which
// override them.
func commandEntries(env func(string) (string, bool)) ([]rawEntry, error) {
	out, err := countEntries(env)
	if err != nil {
		return nil, err
	}
	if params, ok := env("GIT_CONFIG_PARAMETERS"); ok {
		more, err := parseParameters(params)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

func countEntries(env func(string) (string, bool)) ([]rawEntry, error) {
	v, _ := env("GIT_CONFIG_COUNT")
	// git reads the count with strtoul: leading whitespace and a sign are
	// accepted, trailing garbage is not, and a negative count wraps to a
	// huge one.
	v = strings.TrimLeft(v, " \t\n\v\f\r")
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, errors.New("bogus count in GIT_CONFIG_COUNT")
	}
	if err != nil || n < 0 || n > math.MaxInt32 {
		return nil, errors.New("too many entries in GIT_CONFIG_COUNT")
	}
	out := make([]rawEntry, 0, n)
	for i := range n {
		keyVar := "GIT_CONFIG_KEY_" + strconv.FormatInt(i, 10)
		valueVar := "GIT_CONFIG_VALUE_" + strconv.FormatInt(i, 10)
		key, ok := env(keyVar)
		if !ok {
			return nil, fmt.Errorf("missing config key %s", keyVar)
		}
		value, ok := env(valueVar)
		if !ok {
			return nil, fmt.Errorf("missing config value %s", valueVar)
		}
		canon, err := CanonicalKey(key)
		if err != nil {
			return nil, err
		}
		out = append(out, rawEntry{key: canon, value: value, hasValue: true})
	}
	return out, nil
}

// parseParameters decodes GIT_CONFIG_PARAMETERS: whitespace-separated
// shell-single-quoted items, each either 'key'='value', 'key'= (no value: a
// bare boolean) or the older 'key=value' form.
func parseParameters(s string) ([]rawEntry, error) {
	bogus := errors.New("bogus format in GIT_CONFIG_PARAMETERS")
	var out []rawEntry
	for {
		s = strings.TrimLeft(s, " \t\n\v\f\r")
		if s == "" {
			return out, nil
		}
		key, rest, ok := unquoteSingle(s)
		if !ok {
			return nil, bogus
		}
		e := rawEntry{}
		if after, isNew := strings.CutPrefix(rest, "="); isNew {
			rest = after
			if rest != "" && !isSpace(rest[0]) {
				value, more, ok := unquoteSingle(rest)
				if !ok {
					return nil, bogus
				}
				e.value, e.hasValue, rest = value, true, more
			}
		} else if k, v, hasEq := strings.Cut(key, "="); hasEq {
			key, e.value, e.hasValue = k, v, true
		}
		if rest != "" && !isSpace(rest[0]) {
			return nil, bogus
		}
		canon, err := CanonicalKey(key)
		if err != nil {
			return nil, err
		}
		e.key = canon
		out = append(out, e)
		s = rest
	}
}

// unquoteSingle reads one shell single-quoted word as git writes it: a
// quoted run in which a quote or '!' is written as: close quote, backslash,
// the character, reopen quote.
func unquoteSingle(s string) (word, rest string, ok bool) {
	var b strings.Builder
	for {
		if s == "" || s[0] != '\'' {
			return "", "", false
		}
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", "", false
		}
		b.WriteString(s[1 : 1+end])
		s = s[end+2:]
		if len(s) >= 3 && s[0] == '\\' && (s[1] == '\'' || s[1] == '!') && s[2] == '\'' {
			b.WriteByte(s[1])
			s = s[2:]
			continue
		}
		return b.String(), s, true
	}
}
