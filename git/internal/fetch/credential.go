package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
)

// credential is one authentication context and what is known of its
// credentials, in the attribute vocabulary of git-credential.
type credential struct {
	protocol, host, path string
	username, password   string
	// wwwauth holds the server's WWW-Authenticate headers, passed to helpers.
	wwwauth []string
	// fromURL marks a password embedded in the URL, never stored or erased.
	fromURL bool
}

// encode writes the attributes for a helper's stdin. A value holding a
// newline or NUL (or, with protectCR, a carriage return) is refused: it
// could smuggle extra attributes such as "host=" into the helper's input
// (CVE-2020-5260), and a URL's percent-decoded user name or path is
// attacker-controlled.
func (c *credential) encode(protectCR bool) ([]byte, error) {
	var b bytes.Buffer
	var err error
	put := func(k, v string) {
		if v == "" || err != nil {
			return
		}
		if err = checkCredentialValue(k, v, protectCR); err == nil {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	put("protocol", c.protocol)
	put("host", c.host)
	put("path", c.path)
	put("username", c.username)
	put("password", c.password)
	for _, w := range c.wwwauth {
		put("wwwauth[]", w)
	}
	return b.Bytes(), err
}

// checkCredentialValue refuses a credential attribute or prompt that
// could break the line-oriented helper protocol.
func checkCredentialValue(what, v string, protectCR bool) error {
	switch {
	case strings.ContainsAny(v, "\n\x00"):
		return fmt.Errorf("fetch: credential %s contains a newline or NUL byte", what)
	case protectCR && strings.Contains(v, "\r"):
		return fmt.Errorf("fetch: credential %s contains a carriage return (see credential.protectProtocol)", what)
	}
	return nil
}

// promptURL is how git names the context in askpass prompts.
func (c *credential) promptURL(withUser bool) string {
	u := c.protocol + "://"
	if withUser && c.username != "" {
		u += url.PathEscape(c.username) + "@"
	}
	u += c.host
	return u
}

// credentialStore consults credential helpers and askpass programs for one
// remote URL, following gitcredentials(7). Terminal prompts never happen.
type credentialStore struct {
	env     environ
	helpers []string // helper specifications, in order
	// protectCR refuses carriage returns in attributes, unless
	// credential.protectProtocol is false.
	protectCR bool
}

// newCredentialStore gathers credential.helper and credential.<url>.helper
// (an empty value resets the list) and credential.<url>.username and
// useHttpPath for u, filling c's context.
func newCredentialStore(cfg *config.Config, env environ, u *url.URL, c *credential) (*credentialStore, error) {
	c.protocol = u.Scheme
	c.host = u.Host
	matchURL := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, User: userOnly(u)}).String()
	s := &credentialStore{env: env, protectCR: true}
	if cfg == nil {
		return s, nil
	}
	if v, found, err := cfg.Bool("credential.protectprotocol"); err != nil {
		return nil, err
	} else if found {
		s.protectCR = v
	}
	entries, err := cfg.URLMatchAll("credential", "helper", matchURL)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Value == "" {
			s.helpers = nil
			continue
		}
		s.helpers = append(s.helpers, e.Value)
	}
	if c.username == "" {
		if e, ok, err := cfg.URLMatch("credential", "username", matchURL); err != nil {
			return nil, err
		} else if ok {
			c.username = e.Value
		}
	}
	if e, ok, err := cfg.URLMatch("credential", "usehttppath", matchURL); err != nil {
		return nil, err
	} else if ok {
		use, err := e.Bool()
		if err != nil {
			return nil, err
		}
		if use {
			c.path = strings.TrimPrefix(u.Path, "/")
		}
	}
	return s, nil
}

func userOnly(u *url.URL) *url.Userinfo {
	if u.User == nil {
		return nil
	}
	return url.User(u.User.Username())
}

// fill completes c's username and password: each helper in turn ("get")
// until both are known or one says quit, then askpass for what is missing.
func (s *credentialStore) fill(ctx context.Context, cfg *config.Config, c *credential) error {
	// Refuse a poisoned context before any helper or askpass runs.
	if _, err := c.encode(s.protectCR); err != nil {
		return err
	}
	for _, h := range s.helpers {
		if c.username != "" && c.password != "" {
			break
		}
		out, err := s.run(ctx, h, "get", c)
		if err != nil {
			return err
		}
		quit := false
		for line := range strings.Lines(string(out)) {
			k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "=")
			if !ok {
				continue
			}
			switch k {
			case "username":
				c.username = v
			case "password":
				c.password = v
			case "quit":
				quit = v == "true" || v == "1"
			}
		}
		if quit {
			if c.username == "" || c.password == "" {
				return fmt.Errorf("fetch: credential helper '%s' told us to quit", h)
			}
			break
		}
	}
	if c.username == "" {
		u, err := s.askpass(ctx, cfg, fmt.Sprintf("Username for '%s': ", c.promptURL(false)))
		if err != nil {
			return fmt.Errorf("fetch: could not read Username for '%s': %w", c.promptURL(false), err)
		}
		c.username = u
	}
	if c.password == "" {
		p, err := s.askpass(ctx, cfg, fmt.Sprintf("Password for '%s': ", c.promptURL(true)))
		if err != nil {
			return fmt.Errorf("fetch: could not read Password for '%s': %w", c.promptURL(true), err)
		}
		c.password = p
	}
	return nil
}

// approve tells every helper to store a credential the server accepted.
func (s *credentialStore) approve(ctx context.Context, c *credential) {
	if c.fromURL || c.username == "" || c.password == "" {
		return
	}
	for _, h := range s.helpers {
		s.run(ctx, h, "store", c) // a helper failing to store is not fatal, as in git
	}
}

// reject tells every helper to erase a credential the server refused.
func (s *credentialStore) reject(ctx context.Context, c *credential) {
	if c.fromURL {
		return
	}
	for _, h := range s.helpers {
		s.run(ctx, h, "erase", c)
	}
}

// errNoAskpass is returned when no helper or askpass program can answer and
// git would fall back to a terminal prompt.
var errNoAskpass = errors.New("terminal prompts disabled")

// askpass runs GIT_ASKPASS, core.askPass or SSH_ASKPASS with prompt as its
// argument and returns its first output line.
func (s *credentialStore) askpass(ctx context.Context, cfg *config.Config, prompt string) (string, error) {
	if err := checkCredentialValue("prompt", prompt, s.protectCR); err != nil {
		return "", err
	}
	prog := s.env.get("GIT_ASKPASS")
	if prog == "" && cfg != nil {
		prog, _ = cfg.Get("core.askpass")
	}
	if prog == "" {
		prog = s.env.get("SSH_ASKPASS")
	}
	if prog == "" {
		return "", errNoAskpass
	}
	cmd := command(ctx, prog, prompt)
	cmd.Env = s.env.childEnv()
	var stderr stderrBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", withStderr(fmt.Errorf("unable to read askpass response from '%s': %w", prog, err), &stderr)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSuffix(line, "\r"), nil
}

// run executes helper spec h with operation op, c's attributes on stdin.
func (s *credentialStore) run(ctx context.Context, h, op string, c *credential) ([]byte, error) {
	cmd, err := s.helperCommand(ctx, h, op)
	if err != nil {
		return nil, err
	}
	cmd.Env = s.env.childEnv()
	input, err := c.encode(s.protectCR)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = bytes.NewReader(input)
	var stderr stderrBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && op == "get" {
		// git ignores a failing helper and moves on to the next one, but a
		// cancelled context must stop the fetch.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	return out, nil
}

// helperCommand turns a credential.helper value into a command running
// operation op: "!snippet" runs the snippet through the shell, an absolute
// path runs verbatim through the shell, and "name args" runs the
// git-credential-name program with args. That program is looked up directly
// instead of running "git credential-name", and needs the shell only when
// args use shell syntax.
func (s *credentialStore) helperCommand(ctx context.Context, h, op string) (*exec.Cmd, error) {
	if snippet, ok := strings.CutPrefix(h, "!"); ok {
		return s.env.shellCommand(ctx, snippet, op)
	}
	if isAbsHelper(h) {
		return s.env.shellCommand(ctx, h, op)
	}
	name, args, _ := strings.Cut(h, " ")
	prog, err := s.findHelper("git-credential-" + name)
	if err != nil {
		return nil, fmt.Errorf("fetch: credential helper '%s': %w", name, err)
	}
	if strings.TrimSpace(args) == "" {
		return command(ctx, prog, op), nil
	}
	if words, ok := plainWords(args); ok {
		return command(ctx, prog, append(words, op)...), nil
	}
	return s.env.shellCommand(ctx, shellQuote(prog)+" "+args, op)
}

func isAbsHelper(h string) bool {
	if filepath.IsAbs(h) {
		return true
	}
	// git also takes a leading slash or drive letter as absolute on Windows.
	return runtime.GOOS == "windows" && (strings.HasPrefix(h, "/") || strings.HasPrefix(h, `\`))
}

// findHelper looks for a helper program in PATH, then $GIT_EXEC_PATH, then
// the libexec/git-core directory of the git installation found on PATH.
func (s *credentialStore) findHelper(name string) (string, error) {
	if p, err := s.env.lookPath(name); err == nil {
		return p, nil
	}
	if dir := s.env.get("GIT_EXEC_PATH"); dir != "" {
		if p, ok := findExecutable(dir, name); ok {
			return p, nil
		}
	}
	if git, err := s.env.lookPath("git"); err == nil {
		if real, err := filepath.EvalSymlinks(git); err == nil {
			// <prefix>/bin/git → <prefix>/libexec/git-core
			dir := filepath.Join(filepath.Dir(filepath.Dir(real)), "libexec", "git-core")
			if p, ok := findExecutable(dir, name); ok {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found in PATH, GIT_EXEC_PATH or git's libexec directory", name)
}

// shellQuote quotes s for sh the way git quotes arguments: single quotes,
// with ' and ! escaped outside them.
func shellQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '!':
			b.WriteString(`'\`)
			b.WriteRune(r)
			b.WriteByte('\'')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}
