package repo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// hostLookupTimeout bounds the canonical host name lookup made when no email
// address is configured.
const hostLookupTimeout = 2 * time.Second

// Committer returns the identity git records as the committer of a ref
// update (the reflog identity), following git-commit-tree's COMMIT
// INFORMATION chain leniently as reflog writes do:
//
//   - name: GIT_COMMITTER_NAME, committer.name, user.name, then the account's
//     full name; an empty result becomes the account's user name.
//   - email: GIT_COMMITTER_EMAIL, committer.email, user.email, EMAIL, then
//     <user>@<host>, the host being /etc/mailname's first line, else the
//     host name if it contains a dot, else its canonical name if that does,
//     else "<hostname>.(none)".
//   - date: GIT_COMMITTER_DATE (git's internal "<unix> <zone>" form,
//     RFC 2822 or ISO 8601), else now.
//
// Set-but-empty environment variables and user.* values count (an empty
// email stays empty); empty committer.* values and an empty EMAIL are
// skipped. Name and email lose '<', '>' and newlines, and leading and
// trailing whitespace and ",:;\"'\\" characters, as git sanitizes them.
func (r *Repository) Committer() (object.Signature, error) {
	cfg, err := r.Config()
	if err != nil {
		return object.Signature{}, err
	}
	return committer(cfg, os.LookupEnv, time.Now())
}

func committer(cfg *config.Config, env func(string) (string, bool), now time.Time) (object.Signature, error) {
	var account *user.User
	lookupAccount := func() (*user.User, error) {
		if account == nil {
			u, err := user.Current()
			if err != nil {
				return nil, fmt.Errorf("repo: cannot determine the current user: %w", err)
			}
			account = u
		}
		return account, nil
	}

	name, ok := env("GIT_COMMITTER_NAME")
	if !ok {
		name, ok = cfg.Get("committer.name")
		ok = ok && name != ""
	}
	if !ok {
		name, ok = cfg.Get("user.name")
	}
	if !ok {
		u, err := lookupAccount()
		if err != nil {
			return object.Signature{}, err
		}
		name = fullName(u)
	}
	if name == "" {
		u, err := lookupAccount()
		if err != nil {
			return object.Signature{}, err
		}
		name = accountName(u)
	}

	email, ok := env("GIT_COMMITTER_EMAIL")
	if !ok {
		email, ok = cfg.Get("committer.email")
		ok = ok && email != ""
	}
	if !ok {
		email, ok = cfg.Get("user.email")
	}
	if !ok {
		email, ok = env("EMAIL")
		ok = ok && email != ""
	}
	if !ok {
		u, err := lookupAccount()
		if err != nil {
			return object.Signature{}, err
		}
		email = accountName(u) + "@" + mailDomain()
	}

	when := now
	if d, ok := env("GIT_COMMITTER_DATE"); ok && d != "" {
		t, err := parseDate(d)
		if err != nil {
			return object.Signature{}, err
		}
		when = t
	}
	return object.Signature{Name: sanitize(name), Email: sanitize(email), When: when}, nil
}

// fullName returns the account's full name: on Unix the first
// comma-separated field of the GECOS field, with '&' standing for the
// capitalized user name.
func fullName(u *user.User) string {
	name, _, _ := strings.Cut(u.Name, ",")
	if strings.Contains(name, "&") {
		login := accountName(u)
		if r, size := utf8.DecodeRuneInString(login); size > 0 {
			login = string(unicode.ToUpper(r)) + login[size:]
		}
		name = strings.ReplaceAll(name, "&", login)
	}
	return name
}

// accountName returns the login name, without a Windows "DOMAIN\" prefix.
func accountName(u *user.User) string {
	if i := strings.LastIndexByte(u.Username, '\\'); i >= 0 {
		return u.Username[i+1:]
	}
	return u.Username
}

// mailDomain returns the host part of a default email address.
func mailDomain() string {
	if f, err := os.Open("/etc/mailname"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		if sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				return line
			}
		}
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "(none)"
	}
	if strings.Contains(host, ".") {
		return host
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostLookupTimeout)
	defer cancel()
	if cname, err := net.DefaultResolver.LookupCNAME(ctx, host); err == nil {
		if cname = strings.TrimSuffix(cname, "."); strings.Contains(cname, ".") {
			return cname
		}
	}
	return host + ".(none)"
}

// sanitize drops '<', '>' and newlines, then trims leading and trailing
// whitespace and ",:;\"'\\" characters.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r == '\n' {
			return -1
		}
		return r
	}, s)
	return strings.TrimFunc(s, func(r rune) bool {
		return r <= ' ' || strings.ContainsRune(",:;\"'\\", r)
	})
}

var (
	reInternalDate = regexp.MustCompile(`^@?([0-9]+)(?:\s+([+-][0-9]{2}(?:[0-9]{2})?))?$`)
	reISODate      = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})[T ]([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.[0-9]+)?\s*(Z|[+-][0-9]{2}(?::?[0-9]{2})?)?$`)
	rfc2822Layouts = []string{
		"Mon, _2 Jan 2006 15:04:05 -0700",
		"_2 Jan 2006 15:04:05 -0700",
		"Mon, _2 Jan 2006 15:04 -0700",
		"_2 Jan 2006 15:04 -0700",
	}
	errDate = errors.New("invalid date format")
)

// parseDate parses GIT_COMMITTER_DATE in the formats git-commit-tree
// documents: "[@]<unix> [<zone>]", RFC 2822 and ISO 8601 (fraction ignored,
// no zone meaning local time). Other spellings git might accept are refused.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if m := reInternalDate.FindStringSubmatch(s); m != nil {
		sec, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: %s", errDate, s)
		}
		t := time.Unix(sec, 0)
		if m[2] == "" {
			return t, nil
		}
		return t.In(zone(m[2])), nil
	}
	if m := reISODate.FindStringSubmatch(s); m != nil {
		n := make([]int, 6)
		for i := range n {
			n[i], _ = strconv.Atoi(m[i+1])
		}
		loc := time.Local
		switch {
		case m[7] == "Z":
			loc = time.FixedZone("", 0)
		case m[7] != "":
			loc = zone(strings.ReplaceAll(m[7], ":", ""))
		}
		return time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, loc), nil
	}
	for _, layout := range rfc2822Layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: %s", errDate, s)
}

// zone turns "+hh" or "+hhmm" into a fixed zone.
func zone(z string) *time.Location {
	hh, _ := strconv.Atoi(z[1:3])
	mm := 0
	if len(z) >= 5 {
		mm, _ = strconv.Atoi(z[3:5])
	}
	offset := (hh*60 + mm) * 60
	if z[0] == '-' {
		offset = -offset
	}
	return time.FixedZone("", offset)
}
