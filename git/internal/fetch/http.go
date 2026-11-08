package fetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
)

const (
	advertisementType = "application/x-git-upload-pack-advertisement"
	requestType       = "application/x-git-upload-pack-request"
	resultType        = "application/x-git-upload-pack-result"
	infoRefsSuffix    = "/info/refs"
)

// redirectPolicy is http.followRedirects.
type redirectPolicy uint8

const (
	redirectInitial redirectPolicy = iota // only the first request (default)
	redirectAlways
	redirectNever
)

// httpSession is one HTTP conversation with a repository: smart (with its
// upload-pack service) or dumb (plain file requests).
type httpSession struct {
	env       environ
	cfg       *config.Config
	matchURL  string   // the URL given to Fetch, for http.<url>.* and credentials
	base      *url.URL // repository URL without credentials, updated by redirects
	userAgent string
	headers   []string // http.extraHeader values, "Name: value"
	redirects redirectPolicy
	transport *http.Transport
	// jar holds cookies when http.cookieFile is set; close writes them to
	// cookieFile when http.saveCookies asks for it.
	jar        *cookieJar
	cookieFile string

	// cred is sent as Basic authentication, but only to credOrigin
	// ("scheme://host:port"): the origin that asked for it with a 401, or
	// the URL that embedded it. Requests anywhere else, redirects
	// included, never carry it.
	cred       credential
	credOrigin string
	store      *credentialStore
	approved   bool
}

// origin is a URL's scheme, host and port, with the default port explicit.
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// newHTTPSession reads the http.* settings that apply to ep's URL, loading
// the client certificate (asking for its passphrase) and cookies they name.
func newHTTPSession(ctx context.Context, cfg *config.Config, env environ, ep *endpoint) (*httpSession, error) {
	u := *ep.url
	s := &httpSession{env: env, cfg: cfg, userAgent: agent}
	if u.User != nil {
		s.cred.username = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			s.cred.password, s.cred.fromURL = pw, true
			s.credOrigin = origin(&u)
		}
	}
	u.User = userOnly(&u) // the password plays no part in matching
	s.matchURL = u.String()
	u.User = nil
	s.base = &u

	get := func(name string) (config.Entry, bool, error) {
		if cfg == nil {
			return config.Entry{}, false, nil
		}
		return cfg.URLMatch("http", name, s.matchURL)
	}
	if cfg != nil {
		entries, err := cfg.URLMatchAll("http", "extraheader", s.matchURL)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Value == "" {
				s.headers = nil // an empty value resets the list
				continue
			}
			s.headers = append(s.headers, e.Value)
		}
	}
	if e, ok, err := get("useragent"); err != nil {
		return nil, err
	} else if ok && e.Value != "" {
		s.userAgent = e.Value
	}
	if v := env.get("GIT_HTTP_USER_AGENT"); v != "" {
		s.userAgent = v
	}
	if e, ok, err := get("followredirects"); err != nil {
		return nil, err
	} else if ok {
		switch {
		case strings.EqualFold(e.Value, "initial"):
			s.redirects = redirectInitial
		default:
			follow, err := e.Bool()
			if err != nil {
				return nil, err
			}
			s.redirects = redirectNever
			if follow {
				s.redirects = redirectAlways
			}
		}
	}

	tlsConf := &tls.Config{}
	verify := true
	if e, ok, err := get("sslverify"); err != nil {
		return nil, err
	} else if ok {
		if verify, err = e.Bool(); err != nil {
			return nil, err
		}
	}
	if _, ok := env.lookup("GIT_SSL_NO_VERIFY"); ok {
		verify = false
	}
	tlsConf.InsecureSkipVerify = !verify
	caFile, err := pathSetting(get, env, "sslcainfo", "GIT_SSL_CAINFO")
	if err != nil {
		return nil, err
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("fetch: http.sslCAInfo: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("fetch: http.sslCAInfo: no certificates in %s", caFile)
		}
		tlsConf.RootCAs = pool
	}
	cc, err := readClientCert(get, env)
	if err != nil {
		return nil, err
	}
	if cc.certFile != "" {
		cert, err := cc.load(ctx, cfg, env)
		if err != nil {
			return nil, err
		}
		// Sent whatever issuers the server names, as curl does.
		tlsConf.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
	}
	if err := s.readCookies(get, env); err != nil {
		return nil, err
	}

	proxy, err := s.proxy(get)
	if err != nil {
		return nil, err
	}
	s.transport = &http.Transport{
		Proxy:               proxy,
		TLSClientConfig:     tlsConf,
		ForceAttemptHTTP2:   true,
		DialContext:         (&net.Dialer{}).DialContext,
		MaxIdleConnsPerHost: 2,
	}
	return s, nil
}

// setting returns an http.* setting overridden by environment variable
// envName, "" when neither is set.
func setting(get func(string) (config.Entry, bool, error), env environ, name, envName string) (string, error) {
	if v := env.get(envName); v != "" {
		return v, nil
	}
	e, _, err := get(name)
	return e.Value, err
}

// pathSetting is setting for a pathname: the configured value has a leading
// "~/" expanded, the variable is taken as is.
func pathSetting(get func(string) (config.Entry, bool, error), env environ, name, envName string) (string, error) {
	if v := env.get(envName); v != "" {
		return v, nil
	}
	e, _, err := get(name)
	return expandHome(e.Value, env), err
}

// readCookies sets up the cookie jar of http.cookieFile: an empty value
// keeps the cookies servers set for the session only, a path also loads
// that file, and http.saveCookies writes them back to it when done.
func (s *httpSession) readCookies(get func(string) (config.Entry, bool, error), env environ) error {
	e, ok, err := get("cookiefile")
	if err != nil || !ok {
		return err
	}
	s.jar = &cookieJar{}
	file := expandHome(e.Value, env)
	if file == "" {
		return nil
	}
	if err := s.jar.load(file); err != nil {
		return err
	}
	if e, ok, err := get("savecookies"); err != nil {
		return err
	} else if ok {
		save, err := e.Bool()
		if err != nil {
			return err
		}
		if save {
			s.cookieFile = file
		}
	}
	return nil
}

// expandHome expands a leading "~/" the way git expands pathname values.
func expandHome(p string, env environ) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home := env.get("HOME"); home != "" {
			return home + "/" + rest
		}
	}
	return p
}

// proxy resolves http.proxy, else the curl environment variables
// (http_proxy for http, HTTPS_PROXY/https_proxy for https, then
// ALL_PROXY/all_proxy, minus NO_PROXY/no_proxy hosts).
func (s *httpSession) proxy(get func(string) (config.Entry, bool, error)) (func(*http.Request) (*url.URL, error), error) {
	var spec string
	configured := false
	if e, ok, err := get("proxy"); err != nil {
		return nil, err
	} else if ok {
		spec, configured = e.Value, true
	}
	if !configured {
		var names []string
		if s.base.Scheme == "https" {
			names = []string{"HTTPS_PROXY", "https_proxy"}
		} else {
			// curl ignores HTTP_PROXY: CGI servers set it from a request header.
			names = []string{"http_proxy"}
		}
		names = append(names, "ALL_PROXY", "all_proxy")
		for _, n := range names {
			if v := s.env.get(n); v != "" {
				spec = v
				break
			}
		}
		if spec != "" && noProxy(s.env, s.base.Hostname()) {
			spec = ""
		}
	}
	if spec == "" {
		return nil, nil
	}
	if !strings.Contains(spec, "://") {
		spec = "http://" + spec
	}
	pu, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("fetch: bad proxy %q: %w", spec, err)
	}
	return http.ProxyURL(pu), nil
}

// noProxy applies NO_PROXY: "*" matches everything, other entries match a
// host equal to them or a subdomain of them.
func noProxy(env environ, host string) bool {
	list := env.get("NO_PROXY")
	if list == "" {
		list = env.get("no_proxy")
	}
	host = strings.ToLower(host)
	for entry := range strings.SplitSeq(list, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if h, _, err := net.SplitHostPort(entry); err == nil {
			entry = h
		}
		entry = strings.TrimPrefix(entry, ".")
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// client returns an HTTP client following redirects when follow says so.
// Redirects from https to http are refused, and a redirect leaving the
// credential's origin drops the Authorization header.
func (s *httpSession) client(follow bool) *http.Client {
	c := &http.Client{Transport: s.transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !follow {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if prev := via[len(via)-1].URL; strings.EqualFold(prev.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("refusing to follow a redirect from https to %s", req.URL.Scheme)
		}
		if origin(req.URL) != s.credOrigin {
			req.Header.Del("Authorization")
		}
		return nil
	}}
	if s.jar != nil {
		c.Jar = s.jar
	}
	return c
}

// close ends the session, writing its cookies back for http.saveCookies.
// Failing to write them does not fail the fetch, as with git.
func (s *httpSession) close() {
	s.transport.CloseIdleConnections()
	if s.cookieFile != "" {
		s.jar.save(s.cookieFile)
	}
}

// do sends a request built by newReq, answering a 401 with credentials from
// the helpers (or askpass) once. The credential is for the origin that sent
// the 401: when a redirect led there, rebase moves the session to it first,
// so the retry, and only requests to that origin, carry the credential. A
// refused credential is erased from the helpers; an accepted one is stored.
func (s *httpSession) do(ctx context.Context, follow bool, newReq func() (*http.Request, error), rebase func(*url.URL) error) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", s.userAgent)
		req.Header.Set("Pragma", "no-cache")
		for _, h := range s.headers {
			name, value, ok := strings.Cut(h, ":")
			if !ok {
				continue
			}
			req.Header.Add(strings.TrimSpace(name), strings.TrimSpace(value))
		}
		authed := s.credOrigin != "" && origin(req.URL) == s.credOrigin
		if authed {
			req.SetBasicAuth(s.cred.username, s.cred.password)
		}
		resp, err := s.client(follow).Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("fetch: %w", err)
		}
		// The credential reached the answering server only if every
		// redirect stayed within its origin.
		authed = authed && origin(resp.Request.URL) == s.credOrigin
		if resp.StatusCode != http.StatusUnauthorized {
			if authed && !s.approved && resp.StatusCode < 300 {
				s.approved = true
				if s.store != nil {
					s.store.approve(ctx, &s.cred)
				}
			}
			return resp, nil
		}
		resp.Body.Close()
		if authed {
			if s.store != nil {
				s.store.reject(ctx, &s.cred)
			}
			return nil, fmt.Errorf("fetch: Authentication failed for '%s'", s.redactedBase())
		}
		if attempt > 0 {
			return nil, fmt.Errorf("fetch: Authentication failed for '%s'", s.redactedBase())
		}
		if err := rebase(resp.Request.URL); err != nil {
			return nil, err
		}
		if err := s.fillCredentials(ctx, resp); err != nil {
			return nil, err
		}
	}
}

// fillCredentials asks the helpers, then askpass, for a credential for the
// origin that answered resp with a 401, and binds it to that origin. A user
// name from the Fetch URL is kept only for the URL's own origin.
func (s *httpSession) fillCredentials(ctx context.Context, resp *http.Response) error {
	authURL := resp.Request.URL
	var cred credential
	if u, err := url.Parse(s.matchURL); err == nil && u.User != nil && origin(u) == origin(authURL) {
		cred.username = u.User.Username()
	}
	store, err := newCredentialStore(s.cfg, s.env, &url.URL{Scheme: authURL.Scheme, Host: authURL.Host, Path: s.base.Path, User: userIfAny(cred.username)}, &cred)
	if err != nil {
		return err
	}
	cred.wwwauth = resp.Header.Values("WWW-Authenticate")
	if err := store.fill(ctx, s.cfg, &cred); err != nil {
		return err
	}
	s.cred, s.store, s.credOrigin, s.approved = cred, store, origin(authURL), false
	return nil
}

// rebaseTo makes the repository URL the one u names: u with the given
// service suffix ("/info/refs", "/git-upload-pack") removed.
func (s *httpSession) rebaseTo(u *url.URL, suffix string) error {
	path, ok := strings.CutSuffix(u.Path, suffix)
	if !ok {
		return fmt.Errorf("fetch: unable to update url base from redirection to %s", redactURL(u.String()))
	}
	nb := *u
	nb.Path, nb.RawPath, nb.RawQuery, nb.Fragment, nb.User = path, "", "", "", nil
	s.base = &nb
	return nil
}

func userIfAny(name string) *url.Userinfo {
	if name == "" {
		return nil
	}
	return url.User(name)
}

func (s *httpSession) redactedBase() string {
	return s.base.String() + "/"
}

// discover performs the smart info/refs request asking for protocol v2 and
// returns a reader positioned at the advertisement. A server answering
// without the smart content type is dumb: p is then nil and body is its
// info/refs file. Redirects of this first request move the base URL for the
// rest of the conversation.
func (s *httpSession) discover(ctx context.Context) (body io.ReadCloser, p *pktReader, err error) {
	rebase := func(u *url.URL) error { return s.rebaseTo(u, infoRefsSuffix) }
	resp, err := s.do(ctx, s.redirects != redirectNever, func() (*http.Request, error) {
		target := s.base.JoinPath("info", "refs")
		target.RawQuery = "service=git-upload-pack"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Git-Protocol", "version=2")
		return req, nil
	}, rebase)
	if err != nil {
		return nil, nil, err
	}
	if err := checkStatus(resp, s.redactedBase()); err != nil {
		return nil, nil, err
	}
	// The follow-up requests go where the redirects led.
	if err := rebase(resp.Request.URL); err != nil {
		resp.Body.Close()
		return nil, nil, err
	}
	if mediaType(resp.Header.Get("Content-Type")) != advertisementType {
		return resp.Body, nil, nil
	}
	p = newPktReader(resp.Body)
	kind, data, err := p.readPacket()
	if err != nil {
		resp.Body.Close()
		return nil, nil, advertisementError(err)
	}
	// A v0 reply starts with the service line; a v2 one may not.
	line := strings.TrimSuffix(string(data), "\n")
	if kind == pktData && line == "# service=git-upload-pack" {
		if kind, _, err := p.readLine(); err != nil || kind != pktFlush {
			resp.Body.Close()
			return nil, nil, fmt.Errorf("%w: no flush-pkt after the service line", errProtocol)
		}
	} else if kind == pktData && strings.HasPrefix(line, "# service=") {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("%w: unexpected service line %q", errProtocol, line)
	} else {
		p.unreadPacket(kind, data)
	}
	return resp.Body, p, nil
}

// get requests u for the dumb protocol, following redirects only when
// http.followRedirects is true. found is false when the server has no such
// file (404 or 410).
func (s *httpSession) get(ctx context.Context, u *url.URL) (body io.ReadCloser, found bool, err error) {
	resp, err := s.do(ctx, s.redirects == redirectAlways, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	}, func(*url.URL) error { return nil }) // the base stays where discovery left it
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		resp.Body.Close()
		return nil, false, nil
	}
	if err := checkStatus(resp, redactURL(u.String())); err != nil {
		return nil, false, err
	}
	return resp.Body, true, nil
}

// post sends one stateless request to the upload-pack service and returns
// a reader over the result.
func (s *httpSession) post(ctx context.Context, body []byte, v2 bool) (*pktReader, io.Closer, error) {
	resp, err := s.do(ctx, s.redirects == redirectAlways, func() (*http.Request, error) {
		target := s.base.JoinPath("git-upload-pack")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", requestType)
		req.Header.Set("Accept", resultType)
		if v2 {
			req.Header.Set("Git-Protocol", "version=2")
		}
		return req, nil
	}, func(u *url.URL) error { return s.rebaseTo(u, "/git-upload-pack") })
	if err != nil {
		return nil, nil, err
	}
	if err := checkStatus(resp, s.redactedBase()); err != nil {
		return nil, nil, err
	}
	if ct := resp.Header.Get("Content-Type"); mediaType(ct) != resultType {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("%w: unexpected content type %q from %s", errProtocol, ct, redactURL(resp.Request.URL.String()))
	}
	return newPktReader(resp.Body), resp.Body, nil
}

func mediaType(ct string) string {
	t, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(t))
}

// errHTTPStatus wraps unexpected HTTP statuses.
var errHTTPStatus = errors.New("unexpected HTTP status")

func checkStatus(resp *http.Response, what string) error {
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("fetch: repository '%s' not found (HTTP %d)", what, resp.StatusCode)
	case http.StatusForbidden:
		return fmt.Errorf("fetch: access to '%s' denied (HTTP 403)", what)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("fetch: %w %s from '%s': redirect not followed (http.followRedirects)", errHTTPStatus, resp.Status, what)
	}
	return fmt.Errorf("fetch: %w %s from '%s'", errHTTPStatus, resp.Status, what)
}
