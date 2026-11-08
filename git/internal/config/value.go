package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/user"
	"runtime"
	"strings"
)

// ErrPrefixUnsupported is returned for pathnames starting with "%(prefix)/":
// git expands them relative to its own installation, which we do not have.
var ErrPrefixUnsupported = errors.New("config: %(prefix)/ paths need git's install prefix and are unsupported")

var (
	errInvalidUnit = errors.New("invalid unit")
	errOutOfRange  = errors.New("out of range")
)

// ParseBool interprets a config value as git's boolean type: true, yes, on
// and false, no, off and the empty string (any case), or an integer (with an
// optional k/m/g unit) that is true when non-zero. A variable written without
// "= value" is true; callers holding an Entry should use Entry.Bool.
func ParseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off", "":
		return false, nil
	}
	n, err := parseInt(value, math.MinInt32, math.MaxInt32)
	if err != nil {
		return false, fmt.Errorf("bad boolean config value '%s'", value)
	}
	return n != 0, nil
}

// ParseInt interprets a config value as git's 64-bit integer type (what
// `git config --int` prints): C strtoimax syntax (leading whitespace, sign,
// 0x hex, leading-0 octal) and an optional k, m or g unit scaling by 1024,
// 1024² or 1024³.
func ParseInt(value string) (int64, error) {
	n, err := parseInt(value, math.MinInt64, math.MaxInt64)
	if err != nil {
		return 0, fmt.Errorf("bad numeric config value '%s': %w", value, err)
	}
	return n, nil
}

func parseInt(s string, lo, hi int64) (int64, error) {
	i := 0
	for i < len(s) && (s[i] == ' ' || '\t' <= s[i] && s[i] <= '\r') { // C isspace, as strtoimax uses
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	base := uint64(10)
	switch {
	case i+2 < len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') && hexDigit(s[i+2]) >= 0:
		base = 16
		i += 2
	case i < len(s) && s[i] == '0':
		base = 8
	}
	start := i
	var mag uint64
	overflow := false
	for ; i < len(s); i++ {
		d := hexDigit(s[i])
		if d < 0 || uint64(d) >= base {
			break
		}
		if mag > (math.MaxUint64-uint64(d))/base {
			overflow = true
		} else {
			mag = mag*base + uint64(d)
		}
	}
	if i == start {
		return 0, errInvalidUnit
	}
	var factor uint64 = 1
	switch strings.ToLower(s[i:]) {
	case "":
	case "k":
		factor = 1 << 10
	case "m":
		factor = 1 << 20
	case "g":
		factor = 1 << 30
	default:
		return 0, errInvalidUnit
	}
	if overflow || mag > math.MaxUint64/factor {
		return 0, errOutOfRange
	}
	mag *= factor
	if neg {
		if mag > uint64(-(lo+1))+1 {
			return 0, errOutOfRange
		}
		if mag == 0 {
			return 0, nil
		}
		return -int64(mag-1) - 1, nil // mag may be 2⁶³, which int64 cannot hold positive
	}
	if mag > uint64(hi) {
		return 0, errOutOfRange
	}
	return int64(mag), nil
}

func hexDigit(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// expandTilde performs git's pathname interpolation: a leading "~" or "~/"
// becomes $HOME, "~user" or "~user/" that user's home directory, and a
// leading "%(prefix)/" is rejected with ErrPrefixUnsupported.
func expandTilde(p string, env func(string) (string, bool)) (string, error) {
	if strings.HasPrefix(p, "%(prefix)/") {
		return "", ErrPrefixUnsupported
	}
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	name, rest, _ := strings.Cut(p[1:], "/")
	slash := len(p) > 1+len(name)
	var home string
	ok := false
	switch {
	case name == "":
		home, ok = homeDir(env)
	case runtime.GOOS != "windows": // Git for Windows never expands "~user".
		if u, err := user.Lookup(name); err == nil {
			home, ok = u.HomeDir, true
		}
	}
	if !ok {
		return "", fmt.Errorf("failed to expand user dir in: '%s'", p)
	}
	if !slash {
		return home, nil
	}
	return home + "/" + rest, nil
}

// expandPath interprets a pathname-typed value: tilde expansion plus the
// ":(optional)" prefix, for which exists reports false when the named path
// does not exist (git then treats the variable as unset).
func expandPath(value string, env func(string) (string, bool)) (p string, exists bool, err error) {
	rest, optional := strings.CutPrefix(value, ":(optional)")
	p, err = expandTilde(rest, env)
	if err != nil {
		return "", false, err
	}
	if optional {
		if _, err := os.Stat(p); err != nil {
			return "", false, nil
		}
	}
	return p, true, nil
}
