package config

import (
	"errors"
	"slices"
	"strconv"
	"strings"
)

// matchURL is a URL normalised for <section>.<url>.<key> matching.
type matchURL struct {
	scheme  string
	user    string
	hasUser bool
	host    string // lower-cased, one trailing '.' dropped
	port    string // "" for the scheme's default
	path    string // from the first '/', '?' or '#' on; always starts with '/'
}

// parseMatchURL normalises a URL the way git does before matching it: scheme
// and host lower-cased, default ports (http 80, https 443) and leading zeros
// dropped, the password ignored, percent-escapes of unreserved characters
// decoded and others upper-cased, characters outside the URL alphabet
// escaped, and "." and ".." path segments resolved. wildcard permits '*' host
// labels, which only config keys may use.
func parseMatchURL(raw string, wildcard bool) (matchURL, error) {
	var u matchURL
	sep := strings.Index(raw, "://")
	if sep <= 0 || !isAlpha(raw[0]) {
		return u, errors.New("invalid URL scheme name or missing '://' suffix")
	}
	for i := range sep {
		if c := raw[i]; !isAlnum(c) && c != '+' && c != '-' && c != '.' {
			return u, errors.New("invalid URL scheme name or missing '://' suffix")
		}
	}
	u.scheme = strings.ToLower(raw[:sep])
	rest := raw[sep+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]

	if at := strings.IndexByte(authority, '@'); at >= 0 {
		user, _, _ := strings.Cut(authority[:at], ":")
		var err error
		if u.user, err = normalizeEscapes(user); err != nil {
			return u, err
		}
		u.hasUser = true
		authority = authority[at+1:]
	}

	host := authority
	port := ""
	hostEnd := 0
	if strings.HasPrefix(host, "[") {
		if i := strings.IndexByte(host, ']'); i >= 0 {
			hostEnd = i + 1
		}
	}
	if i := strings.LastIndexByte(host[hostEnd:], ':'); i >= 0 {
		host, port = authority[:hostEnd+i], authority[hostEnd+i+1:]
	}
	if host == "" && u.scheme != "file" {
		return u, errors.New("missing host and scheme is not 'file:'")
	}
	for i := range len(host) {
		c := host[i]
		if !isAlnum(c) && !strings.ContainsRune("-._[]:", rune(c)) && !(wildcard && c == '*') {
			return u, errors.New("invalid characters in host name")
		}
	}
	host = strings.ToLower(host)
	if len(host) > 1 {
		host = strings.TrimSuffix(host, ".")
	}
	u.host = host

	if port != "" {
		n, err := strconv.ParseUint(port, 10, 32)
		if err != nil || n == 0 || n > 65535 {
			return u, errors.New("invalid port number")
		}
		port = strconv.FormatUint(n, 10)
		if (u.scheme == "http" && port == "80") || (u.scheme == "https" && port == "443") {
			port = ""
		}
	}
	u.port = port

	p, err := normalizePath(tail)
	if err != nil {
		return u, err
	}
	u.path = p
	return u, nil
}

// normalizePath normalises the part of a URL after the authority. Dot
// segments are resolved in the path proper; the query and fragment are only
// escape-normalised.
func normalizePath(tail string) (string, error) {
	pathPart, query := tail, ""
	if i := strings.IndexAny(tail, "?#"); i >= 0 {
		pathPart, query = tail[:i], tail[i:]
	}
	pathPart, err := normalizeEscapes(pathPart)
	if err != nil {
		return "", err
	}
	if query, err = normalizeEscapes(query); err != nil {
		return "", err
	}
	if !strings.HasPrefix(pathPart, "/") {
		pathPart = "/" + pathPart
	}
	var out []string
	segments := strings.Split(pathPart[1:], "/")
	for i, seg := range segments {
		last := i == len(segments)-1
		switch seg {
		case ".":
			if last {
				out = append(out, "")
			}
		case "..":
			if len(out) == 0 {
				return "", errors.New("invalid '..' path segment")
			}
			out = out[:len(out)-1]
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/") + query, nil
}

// normalizeEscapes decodes %XX escapes of unreserved characters, upper-cases
// the hex of the rest, and escapes bytes that may not appear in a URL.
func normalizeEscapes(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' {
			if i+2 >= len(s) || hexDigit(s[i+1]) < 0 || hexDigit(s[i+2]) < 0 {
				return "", errors.New("invalid %XX escape sequence")
			}
			d := byte(hexDigit(s[i+1])<<4 | hexDigit(s[i+2]))
			i += 2
			if unreserved(d) {
				b.WriteByte(d)
			} else {
				writeEscape(&b, d)
			}
			continue
		}
		if unreserved(c) || strings.IndexByte(":/?#[]@!$&'()*+,;=", c) >= 0 {
			b.WriteByte(c)
		} else {
			writeEscape(&b, c)
		}
	}
	return b.String(), nil
}

func unreserved(c byte) bool {
	return isAlnum(c) || c == '-' || c == '.' || c == '_' || c == '~'
}

func writeEscape(b *strings.Builder, c byte) {
	const hex = "0123456789ABCDEF"
	b.WriteByte('%')
	b.WriteByte(hex[c>>4])
	b.WriteByte(hex[c&15])
}

// urlSpecificity orders matches: a longer matching path wins, then a key
// naming a user; a plain <section>.<key> ranks below every URL match.
type urlSpecificity struct {
	pathLen int
	user    bool
}

func (a urlSpecificity) less(b urlSpecificity) bool {
	if a.pathLen != b.pathLen {
		return a.pathLen < b.pathLen
	}
	return !a.user && b.user
}

var plainSpecificity = urlSpecificity{pathLen: -1}

// matches reports whether config URL k applies to u and how specifically.
func (k matchURL) matches(u matchURL) (urlSpecificity, bool) {
	if k.scheme != u.scheme || k.port != u.port || !hostMatches(k.host, u.host) {
		return urlSpecificity{}, false
	}
	if k.hasUser && (!u.hasUser || k.user != u.user) {
		return urlSpecificity{}, false
	}
	// A trailing '/' on the key's path is not significant: "/foo/" and
	// "/foo" both match "/foo" and everything below it.
	kp := strings.TrimSuffix(k.path, "/")
	if u.path != kp && !strings.HasPrefix(u.path, kp+"/") {
		return urlSpecificity{}, false
	}
	return urlSpecificity{pathLen: len(kp), user: k.hasUser}, true
}

// hostMatches compares host names label by label; a "*" label in the
// pattern matches any one label.
func hostMatches(pattern, host string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == host
	}
	pl := strings.Split(pattern, ".")
	hl := strings.Split(host, ".")
	if len(pl) != len(hl) {
		return false
	}
	for i, l := range pl {
		if l != "*" && l != hl[i] {
			return false
		}
	}
	return true
}

// urlCandidates yields, for entries of section, those that apply to u with
// their specificity: <section>.<name> always, <section>.<url>.<name> when
// <url> matches. name "" accepts every variable.
func (c *Config) urlCandidates(section, name string, u matchURL, yield func(Entry, urlSpecificity)) {
	section, name = strings.ToLower(section), strings.ToLower(name)
	keys := map[string]*matchURL{} // parsed config URLs; nil when unparsable
	for _, e := range c.entries {
		s, sub, hasSub, n := splitKey(e.Key)
		if s != section || (name != "" && n != name) {
			continue
		}
		if !hasSub {
			yield(e, plainSpecificity)
			continue
		}
		k, seen := keys[sub]
		if !seen {
			if parsed, err := parseMatchURL(sub, true); err == nil {
				k = &parsed
			}
			keys[sub] = k
		}
		if k == nil {
			continue
		}
		if spec, ok := k.matches(u); ok {
			yield(e, spec)
		}
	}
}

// URLMatch returns the entry for <section>.<name> that applies to rawURL,
// like `git config --get-urlmatch <section>.<name> <url>`: the
// <section>.<url>.<name> whose <url> matches best (scheme, host with "*."
// wildcards, port, longest path prefix, then user name; later entries win
// ties), else the last plain <section>.<name>. It fails only for an
// unparsable rawURL.
func (c *Config) URLMatch(section, name, rawURL string) (Entry, bool, error) {
	u, err := parseMatchURL(rawURL, false)
	if err != nil {
		return Entry{}, false, err
	}
	var best Entry
	var bestSpec urlSpecificity
	found := false
	c.urlCandidates(section, name, u, func(e Entry, spec urlSpecificity) {
		if !found || !spec.less(bestSpec) {
			best, bestSpec, found = e, spec, true
		}
	})
	return best, found, nil
}

// URLMatchAll returns every <section>.<name> entry and every
// <section>.<url>.<name> entry whose <url> matches rawURL, in precedence
// order: how git gathers multi-valued settings such as credential.helper.
func (c *Config) URLMatchAll(section, name, rawURL string) ([]Entry, error) {
	u, err := parseMatchURL(rawURL, false)
	if err != nil {
		return nil, err
	}
	var out []Entry
	c.urlCandidates(section, name, u, func(e Entry, _ urlSpecificity) { out = append(out, e) })
	return out, nil
}

// URLMatchSection returns, for every variable name in section, the entry
// URLMatch would choose for rawURL, sorted by name, like
// `git config --get-urlmatch <section> <url>`.
func (c *Config) URLMatchSection(section, rawURL string) ([]Entry, error) {
	u, err := parseMatchURL(rawURL, false)
	if err != nil {
		return nil, err
	}
	type pick struct {
		e    Entry
		spec urlSpecificity
	}
	best := map[string]pick{}
	c.urlCandidates(section, "", u, func(e Entry, spec urlSpecificity) {
		n := e.Name()
		if b, ok := best[n]; !ok || !spec.less(b.spec) {
			best[n] = pick{e, spec}
		}
	})
	out := make([]Entry, 0, len(best))
	for _, p := range best {
		out = append(out, p.e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}
