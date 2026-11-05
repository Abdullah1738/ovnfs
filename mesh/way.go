package mesh

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

// Way is one way to reach a node: a UDP host and port, or through the relay.
type Way struct {
	Relay bool   // through the node's relay; Host and Port unused
	Host  string // DNS name or IP literal (IPv6 without brackets)
	Port  uint16
}

// Errors ParseWay reports.
var (
	ErrWayScheme = errors.New("mesh: way is neither udp:host:port nor relay")
	ErrWayNoPort = errors.New("mesh: way has no port")
	ErrWayNoHost = errors.New("mesh: way has no host")
	ErrWayHost   = errors.New("mesh: way host is malformed (IPv6 must be bracketed)")
	ErrWayPort   = errors.New("mesh: way port is not a number from 1 to 65535")
)

// WayOf is the direct way to addr (an IPv4-mapped address as IPv4).
func WayOf(addr netip.AddrPort) Way {
	return Way{Host: addr.Addr().Unmap().WithZone("").String(), Port: addr.Port()}
}

// ParseWay parses "udp:host:port", "udp:[v6]:port", "relay", or the retired
// "iroh:<node id>[@…]" (a relay way).
func ParseWay(s string) (Way, error) {
	if rest, ok := strings.CutPrefix(s, "udp:"); ok {
		at := strings.LastIndexByte(rest, ':')
		if at < 0 {
			return Way{}, ErrWayNoPort
		}
		host, port := rest[:at], rest[at+1:]
		bracketed := len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']'
		if bracketed {
			host = host[1 : len(host)-1]
		}
		if host == "" {
			return Way{}, ErrWayNoHost
		}
		if strings.ContainsAny(host, "[]") || strings.Contains(host, ":") != bracketed ||
			strings.IndexFunc(host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return Way{}, ErrWayHost
		}
		n, err := strconv.ParseUint(strings.TrimPrefix(port, "+"), 10, 16)
		if err != nil || n == 0 || port == "" || port[0] == '-' {
			return Way{}, ErrWayPort
		}
		return Way{Host: host, Port: uint16(n)}, nil
	}
	if s == "relay" {
		return Way{Relay: true}, nil
	}
	if rest, ok := strings.CutPrefix(s, "iroh:"); ok {
		id, _, _ := strings.Cut(rest, "@")
		if _, err := ParseNodeID(id); err != nil {
			return Way{}, err
		}
		return Way{Relay: true}, nil
	}
	return Way{}, ErrWayScheme
}

// String is the way as ParseWay reads it.
func (w Way) String() string {
	switch {
	case w.Relay:
		return "relay"
	case strings.Contains(w.Host, ":"):
		return "udp:[" + w.Host + "]:" + strconv.Itoa(int(w.Port))
	default:
		return "udp:" + w.Host + ":" + strconv.Itoa(int(w.Port))
	}
}

// MarshalText encodes the way as String does.
func (w Way) MarshalText() ([]byte, error) { return []byte(w.String()), nil }

// UnmarshalText parses the way as ParseWay does.
func (w *Way) UnmarshalText(text []byte) error {
	parsed, err := ParseWay(string(text))
	if err != nil {
		return err
	}
	*w = parsed
	return nil
}
