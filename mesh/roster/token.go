package roster

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/stencil-hq/ovnfs/mesh"
)

// tokenPrefix starts every join token (plan 10's `o2://join?…`).
const tokenPrefix = "o2://join?"

// tokenWays caps the direct ways a token carries, keeping it pasteable; the
// joiner learns the rest from the roster.
const tokenWays = 4

// Token is what `mesh setup` prints and `mesh join` takes: which mesh, the
// inviter's key (pinned by the joiner), where to reach it and a ticket secret.
type Token struct {
	Mesh   string
	Node   mesh.NodeID
	Ways   []mesh.Way // the inviter's direct ways, the first tokenWays printed; the mesh's relay is tried too
	Relay  string     // the mesh's relay; "" = production, "off" = none
	Secret [32]byte
}

// String renders the token as one copy-pasteable line:
// o2://join?mesh=<id>&node=<base32>&ways=<way>,<way>&relay=<url>&s=<secret>.
func (t Token) String() string {
	var b strings.Builder
	b.WriteString(tokenPrefix)
	b.WriteString("mesh=" + t.Mesh)
	b.WriteString("&node=" + t.Node.Base32())
	var ways []string
	for _, w := range t.Ways {
		if !w.Relay && len(ways) < tokenWays {
			ways = append(ways, w.String())
		}
	}
	if len(ways) > 0 {
		b.WriteString("&ways=" + strings.Join(ways, ","))
	}
	if t.Relay != "" {
		b.WriteString("&relay=" + url.QueryEscape(t.Relay))
	}
	b.WriteString("&s=" + base64.RawURLEncoding.EncodeToString(t.Secret[:]))
	return b.String()
}

// ParseToken reads a token as String writes it; surrounding space is ignored.
func ParseToken(s string) (Token, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), tokenPrefix)
	if !ok {
		return Token{}, fmt.Errorf("a join token starts with %s", tokenPrefix)
	}
	q, err := url.ParseQuery(rest)
	if err != nil {
		return Token{}, fmt.Errorf("join token: %w", err)
	}
	for key, vals := range q {
		if len(vals) > 1 {
			return Token{}, fmt.Errorf("join token: %s given twice", key)
		}
	}
	var t Token
	if t.Mesh = q.Get("mesh"); len(t.Mesh) != 16 || !isHex(t.Mesh) {
		return Token{}, errors.New("join token: bad mesh id")
	}
	if t.Node, err = mesh.ParseNodeID(q.Get("node")); err != nil {
		return Token{}, fmt.Errorf("join token: node: %w", err)
	}
	if ways := q.Get("ways"); ways != "" {
		for w := range strings.SplitSeq(ways, ",") {
			way, err := mesh.ParseWay(w)
			if err != nil {
				return Token{}, fmt.Errorf("join token: way %q: %w", w, err)
			}
			t.Ways = append(t.Ways, way)
		}
		if len(t.Ways) > maxWays {
			return Token{}, errors.New("join token: too many ways")
		}
	}
	if t.Relay = q.Get("relay"); t.Relay != "" && t.Relay != "off" {
		u, err := url.Parse(t.Relay)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Token{}, fmt.Errorf("join token: bad relay %q", t.Relay)
		}
	}
	secret, err := base64.RawURLEncoding.DecodeString(q.Get("s"))
	if err != nil || len(secret) != len(t.Secret) {
		return Token{}, errors.New("join token: bad secret")
	}
	t.Secret = [32]byte(secret)
	return t, nil
}

// isHex reports whether s is lowercase hex.
func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
