package fetch

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cookieJar is an HTTP session's cookie store when http.cookieFile is set:
// cookies read from that file (the Netscape format curl writes, or
// Set-Cookie header lines) and those servers set, sent back to the requests
// they match (RFC 6265 domain, path and secure rules). With
// http.saveCookies, save writes them back in curl's format.
type cookieJar struct {
	mu      sync.Mutex
	cookies []*jarCookie // in order of creation
}

type jarCookie struct {
	name, value string
	// domain is lower case without a leading dot; hostOnly cookies go to
	// that host only, others to its subdomains too.
	domain   string
	hostOnly bool
	path     string
	secure   bool
	httpOnly bool
	expires  time.Time // zero for a session cookie
}

func (c *jarCookie) expired(now time.Time) bool {
	return !c.expires.IsZero() && !c.expires.After(now)
}

// cookieHost is the host a URL's cookies are matched against.
func cookieHost(u *url.URL) string {
	h := strings.ToLower(u.Hostname())
	if len(h) > 1 {
		h = strings.TrimSuffix(h, ".")
	}
	return h
}

// secureOrigin reports where secure cookies may go: https, and loopback
// hosts, as curl allows.
func secureOrigin(u *url.URL, host string) bool {
	if strings.EqualFold(u.Scheme, "https") || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *jarCookie) domainMatch(host string) bool {
	if host == c.domain {
		return true
	}
	return !c.hostOnly && strings.HasSuffix(host, "."+c.domain) && net.ParseIP(host) == nil
}

// pathMatch implements RFC 6265 section 5.1.4.
func pathMatch(reqPath, cookiePath string) bool {
	if !strings.HasPrefix(reqPath, cookiePath) {
		return false
	}
	return len(reqPath) == len(cookiePath) || strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'
}

// defaultCookiePath is the path of a cookie set without one: the request
// path up to, not including, its last slash.
func defaultCookiePath(u *url.URL) string {
	p := u.EscapedPath()
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// Cookies implements http.CookieJar: the unexpired cookies matching u,
// longest path first.
func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie {
	host := cookieHost(u)
	secure := secureOrigin(u, host)
	reqPath := u.EscapedPath()
	if reqPath == "" {
		reqPath = "/"
	}
	now := time.Now()
	j.mu.Lock()
	var match []*jarCookie
	for _, c := range j.cookies {
		if !c.expired(now) && c.domainMatch(host) && pathMatch(reqPath, c.path) && (secure || !c.secure) {
			match = append(match, c)
		}
	}
	j.mu.Unlock()
	slices.SortStableFunc(match, func(a, b *jarCookie) int { return len(b.path) - len(a.path) })
	out := make([]*http.Cookie, len(match))
	for i, c := range match {
		out[i] = &http.Cookie{Name: c.name, Value: c.value}
	}
	return out
}

// SetCookies implements http.CookieJar, storing the cookies a response to
// u set. A Domain attribute must domain-match u's host and name more than a
// top-level domain; for an IP address host it is ignored, as curl does. A
// secure cookie is refused from an origin that could not receive it back.
func (j *cookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	host := cookieHost(u)
	secure := secureOrigin(u, host)
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, hc := range cookies {
		if hc.Secure && !secure {
			continue
		}
		c := &jarCookie{name: hc.Name, value: hc.Value, secure: hc.Secure, httpOnly: hc.HttpOnly, domain: host, hostOnly: true, path: hc.Path}
		if d := strings.ToLower(strings.TrimPrefix(hc.Domain, ".")); d != "" && net.ParseIP(host) == nil {
			if d != host && (!strings.HasSuffix(host, "."+d) || !strings.Contains(d, ".")) {
				continue
			}
			c.domain, c.hostOnly = d, false
		}
		if !strings.HasPrefix(c.path, "/") {
			c.path = defaultCookiePath(u)
		}
		switch {
		case hc.MaxAge < 0:
			c.expires = now
		case hc.MaxAge > 0:
			c.expires = now.Add(time.Duration(hc.MaxAge) * time.Second)
		default:
			c.expires = hc.Expires
		}
		j.put(c, now)
	}
}

// put stores c, replacing the cookie of the same name, domain and path; an
// expired c deletes it.
func (j *cookieJar) put(c *jarCookie, now time.Time) {
	i := slices.IndexFunc(j.cookies, func(o *jarCookie) bool {
		return o.name == c.name && o.domain == c.domain && o.path == c.path
	})
	switch {
	case c.expired(now):
		if i >= 0 {
			j.cookies = slices.Delete(j.cookies, i, i+1)
		}
	case i >= 0:
		j.cookies[i] = c
	default:
		j.cookies = append(j.cookies, c)
	}
}

// load reads a cookie file: Netscape lines ("domain flag path secure expiry
// name value", "#HttpOnly_" before the domain of HTTP-only cookies) and
// "Set-Cookie:" header lines, which need a Domain attribute to apply
// anywhere. A missing file holds no cookies, as with curl.
func (j *cookieJar) load(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch: http.cookieFile: %w", err)
	}
	now := time.Now()
	for line := range strings.Lines(string(data)) {
		line = strings.TrimRight(line, "\r\n")
		if header, ok := cutPrefixFold(line, "Set-Cookie:"); ok {
			hc, err := http.ParseSetCookie(strings.TrimSpace(header))
			if err != nil || hc.Domain == "" {
				continue
			}
			u := &url.URL{Scheme: "https", Host: strings.TrimPrefix(hc.Domain, "."), Path: hc.Path}
			if u.Path == "" {
				u.Path = "/"
			}
			j.SetCookies(u, []*http.Cookie{hc})
			continue
		}
		httpOnly := false
		if rest, ok := strings.CutPrefix(line, "#HttpOnly_"); ok {
			line, httpOnly = rest, true
		} else if line == "" || line[0] == '#' {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) == 6 {
			f = append(f, "") // a cookie without a value
		}
		if len(f) != 7 {
			continue
		}
		expiry, err := strconv.ParseInt(f[4], 10, 64)
		if err != nil {
			continue
		}
		c := &jarCookie{
			name: f[5], value: f[6],
			domain:   strings.ToLower(strings.TrimPrefix(f[0], ".")),
			hostOnly: !strings.EqualFold(f[1], "TRUE"),
			path:     f[2],
			secure:   strings.EqualFold(f[3], "TRUE"),
			httpOnly: httpOnly,
		}
		if expiry != 0 {
			c.expires = time.Unix(expiry, 0)
		}
		if c.domain != "" && strings.HasPrefix(c.path, "/") {
			j.put(c, now)
		}
	}
	return nil
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// save writes the unexpired cookies to path in the Netscape format curl
// writes, session cookies with expiry 0, replacing the file atomically.
func (j *cookieJar) save(path string) error {
	var b bytes.Buffer
	b.WriteString("# Netscape HTTP Cookie File\n# https://curl.se/docs/http-cookies.html\n# This file was generated by ovnfs.\n\n")
	now := time.Now()
	j.mu.Lock()
	for _, c := range j.cookies {
		if c.expired(now) {
			continue
		}
		domain, sub := c.domain, "FALSE"
		if !c.hostOnly {
			domain, sub = "."+domain, "TRUE"
		}
		if c.httpOnly {
			domain = "#HttpOnly_" + domain
		}
		secure := "FALSE"
		if c.secure {
			secure = "TRUE"
		}
		var expiry int64
		if !c.expires.IsZero() {
			expiry = c.expires.Unix()
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", domain, sub, c.path, secure, expiry, c.name, c.value)
	}
	j.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cookies-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b.Bytes())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
