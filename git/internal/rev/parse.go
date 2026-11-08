package rev

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// hint is the kind of object the context expects, which disambiguates
// short object names as in git (`abcd^{tree}`, `abcd~2`, `abcd:path`).
type hint uint8

const (
	hintNone hint = iota
	hintCommit
	hintTree
	hintBlob
	hintTag
)

// minAbbrev is the shortest abbreviated object name git accepts.
const minAbbrev = 4

// maxNesting bounds how deeply revision expressions may refer to each other
// (each @{-N}, @{upstream} or reflog step resolves a further expression).
const maxNesting = 64

type resolver struct {
	src   *Source
	opts  Options
	w     *walker
	depth int
}

func newResolver(src *Source, opts Options) *resolver {
	return &resolver{src: src, opts: opts, w: newWalker(src, false)}
}

func unknown(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnknown, fmt.Sprintf(format, args...))
}

// resolve evaluates a complete expression: :/<text>, :<n>:<path>,
// <rev>:<path> or a revision with suffixes.
func (r *resolver) resolve(spec string, h hint) (object.ID, error) {
	r.depth++
	defer func() { r.depth-- }()
	if r.depth > maxNesting {
		return object.ID{}, unknown("expression nests too deeply")
	}
	if rest, ok := strings.CutPrefix(spec, ":"); ok {
		if text, ok := strings.CutPrefix(rest, "/"); ok && text != "" {
			return r.searchAll(text)
		}
		stage := uint8(0)
		if len(rest) >= 2 && rest[0] >= '0' && rest[0] <= '3' && rest[1] == ':' {
			stage = rest[0] - '0'
			rest = rest[2:]
		}
		return r.indexPath(rest, stage)
	}
	if i := pathColon(spec); i >= 0 {
		tree, err := r.resolveRev(spec[:i], hintTree)
		if err != nil {
			return object.ID{}, err
		}
		return r.treePath(tree, spec[:i], spec[i+1:])
	}
	return r.resolveRev(spec, h)
}

// pathColon finds the ':' starting <path> in <rev>:<path>, skipping colons
// inside @{...} and ^{...} (dates, regexes).
func pathColon(spec string) int {
	depth := 0
	for i := 0; i < len(spec); i++ {
		switch c := spec[i]; {
		case (c == '@' || c == '^') && i+1 < len(spec) && spec[i+1] == '{':
			depth++
			i++
		case c == '}' && depth > 0:
			depth--
		case c == ':' && depth == 0:
			return i
		}
	}
	return -1
}

// resolveRev evaluates a revision and its trailing ^{type}, ^N and ~N
// suffixes, innermost first.
func (r *resolver) resolveRev(spec string, h hint) (object.ID, error) {
	if strings.HasSuffix(spec, "}") {
		if i := strings.LastIndex(spec, "^{"); i >= 0 && i > strings.LastIndex(spec, "@{") {
			return r.peelSuffix(spec[:i], spec[i+2:len(spec)-1], h)
		}
	}
	j := len(spec)
	for j > 0 && spec[j-1] >= '0' && spec[j-1] <= '9' {
		j--
	}
	if j > 0 && (spec[j-1] == '^' || spec[j-1] == '~') {
		op := spec[j-1]
		n := 1
		if j < len(spec) {
			v, err := strconv.Atoi(spec[j:])
			if err != nil {
				return object.ID{}, unknown("bad count in %q", spec)
			}
			n = v
		}
		base, err := r.resolveRev(spec[:j-1], hintCommit)
		if err != nil {
			return object.ID{}, err
		}
		commit, err := r.peel(base, object.Commit, spec[:j-1])
		if err != nil {
			return object.ID{}, err
		}
		if op == '^' {
			return r.nthParent(commit, n)
		}
		for range n {
			if commit, err = r.nthParent(commit, 1); err != nil {
				return object.ID{}, err
			}
		}
		return commit, nil
	}
	if strings.HasSuffix(spec, "^@") || strings.HasSuffix(spec, "^!") || strings.HasSuffix(spec, "^-") ||
		strings.Contains(spec, "..") {
		return object.ID{}, fmt.Errorf("%w: %q", ErrUnsupported, spec)
	}
	return r.base(spec, h)
}

func (r *resolver) peelSuffix(baseSpec, what string, h hint) (object.ID, error) {
	inner := hintNone
	var want object.Kind
	switch {
	case what == "commit":
		inner, want = hintCommit, object.Commit
	case what == "tree":
		inner, want = hintTree, object.Tree
	case what == "blob":
		// git does not disambiguate short names by ^{blob} or ^{tag}.
		want = object.Blob
	case what == "tag", what == "object", what == "":
	case strings.HasPrefix(what, "/"):
		inner = hintCommit
	default:
		return object.ID{}, unknown("unknown peel type %q", what)
	}
	if inner == hintNone {
		inner = h
	}
	base, err := r.resolveRev(baseSpec, inner)
	if err != nil {
		return object.ID{}, err
	}
	switch {
	case want != 0:
		return r.peel(base, want, baseSpec)
	case what == "tag":
		kind, _, err := r.src.Objects.Header(base)
		if err != nil {
			return object.ID{}, r.missing(base, err)
		}
		if kind != object.Tag {
			return object.ID{}, fmt.Errorf("%w: %s^{tag}: expected tag type, but the object dereferences to %s type", ErrPeel, baseSpec, kind)
		}
		return base, nil
	case what == "object":
		if !r.src.Objects.Has(base) {
			return object.ID{}, r.missing(base, fs.ErrNotExist)
		}
		return base, nil
	case what == "":
		return r.peelTags(base)
	}
	commit, err := r.peel(base, object.Commit, baseSpec)
	if err != nil {
		return object.ID{}, err
	}
	re, negate, err := compileSearch(what[1:])
	if err != nil {
		return object.ID{}, err
	}
	return r.search([]object.ID{commit}, re, negate)
}

func (r *resolver) missing(id object.ID, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return unknown("object %s does not exist", id)
	}
	return err
}

// peelTags follows annotated tags to the first non-tag object.
func (r *resolver) peelTags(id object.ID) (object.ID, error) {
	for range maxPeel {
		kind, data, err := r.src.Objects.Read(id)
		if err != nil {
			return object.ID{}, r.missing(id, err)
		}
		if kind != object.Tag {
			return id, nil
		}
		tag, err := object.ParseTag(r.src.Objects.Format(), data)
		if err != nil {
			return object.ID{}, err
		}
		id = tag.Object
	}
	return object.ID{}, unknown("tag chain too long at %s", id)
}

// maxPeel bounds tag chains, which only corruption could make cyclic.
const maxPeel = 1000

// peel follows tags (and, for want Tree, a commit to its tree) until an
// object of kind want.
func (r *resolver) peel(id object.ID, want object.Kind, spec string) (object.ID, error) {
	for range maxPeel {
		kind, data, err := r.src.Objects.Read(id)
		if err != nil {
			return object.ID{}, r.missing(id, err)
		}
		switch {
		case kind == want:
			return id, nil
		case kind == object.Tag:
			tag, err := object.ParseTag(r.src.Objects.Format(), data)
			if err != nil {
				return object.ID{}, err
			}
			id = tag.Object
			continue
		case kind == object.Commit && want == object.Tree:
			c, err := object.ParseCommit(r.src.Objects.Format(), data)
			if err != nil {
				return object.ID{}, err
			}
			return c.Tree, nil
		}
		return object.ID{}, fmt.Errorf("%w: %s^{%s}: expected %s type, but the object dereferences to %s type", ErrPeel, spec, want, want, kind)
	}
	return object.ID{}, unknown("tag chain too long at %s", id)
}

// nthParent returns commit's n-th parent (n=0: commit itself). Shallow
// commits have none.
func (r *resolver) nthParent(commit object.ID, n int) (object.ID, error) {
	if n == 0 {
		return commit, nil
	}
	parents, err := r.w.parentIDs(commit)
	if err != nil {
		return object.ID{}, err
	}
	if n > len(parents) {
		return object.ID{}, unknown("commit %s has no parent %d", commit, n)
	}
	return parents[n-1], nil
}

// base evaluates a revision without ^/~ suffixes: reflog and branch
// suffixes (@{...}), @, full and abbreviated object names, ref names and
// git-describe output.
func (r *resolver) base(spec string, h hint) (object.ID, error) {
	if spec == "" {
		return object.ID{}, unknown("empty revision")
	}
	if strings.HasSuffix(spec, "}") {
		if at := strings.LastIndex(spec, "@{"); at >= 0 {
			return r.atSuffix(spec[:at], spec[at+2:len(spec)-1])
		}
	}
	if spec == "@" {
		spec = "HEAD"
	}
	f := r.src.Objects.Format()
	if len(spec) == f.HexSize() && isHex(spec) {
		// A full object name is taken as is, existing or not, as git does.
		return object.ParseIDFormat(f, strings.ToLower(spec))
	}
	if full, err := r.expandRef(spec); err == nil {
		ref, err := r.src.Refs.Resolve(full)
		if err != nil {
			return object.ID{}, err
		}
		return ref.ID, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return object.ID{}, err
	}
	if len(spec) >= minAbbrev && len(spec) < f.HexSize() && isHex(spec) {
		return r.abbrev(spec, h)
	}
	if m := describeRE.FindStringSubmatch(spec); m != nil && len(m[1]) >= minAbbrev && len(m[1]) <= f.HexSize() {
		if len(m[1]) == f.HexSize() {
			return object.ParseIDFormat(f, strings.ToLower(m[1]))
		}
		return r.abbrev(m[1], hintCommit)
	}
	return object.ID{}, unknown("%q", spec)
}

// describeRE matches git describe output, <tag>-<n>-g<hex>.
var describeRE = regexp.MustCompile(`-[0-9]+-g([0-9a-fA-F]+)$`)

func isHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// expandRef applies the gitrevisions <refname> rules. Names git would not
// accept as refs (they cannot exist) fail with fs.ErrNotExist.
func (r *resolver) expandRef(name string) (string, error) {
	if strings.Contains(name, "@{") || name == "" {
		return "", fs.ErrNotExist
	}
	full, _, err := r.src.Refs.Expand(name)
	if errors.Is(err, refs.ErrInvalidName) {
		return "", fs.ErrNotExist
	}
	return full, err
}

// abbrev resolves an abbreviated object name; when several objects share
// the prefix, the expected kind h (or core.disambiguate) picks among them.
func (r *resolver) abbrev(prefix string, h hint) (object.ID, error) {
	ids, err := r.src.Objects.FindPrefix(prefix, 0)
	if err != nil {
		return object.ID{}, err
	}
	switch len(ids) {
	case 0:
		return object.ID{}, unknown("%q", prefix)
	case 1:
		return ids[0], nil
	}
	if h == hintNone && r.src.Config != nil {
		if v, ok := r.src.Config.Get("core.disambiguate"); ok {
			switch strings.ToLower(v) {
			case "commit", "committish":
				h = hintCommit
			case "tree", "treeish":
				h = hintTree
			case "blob":
				h = hintBlob
			case "tag":
				h = hintTag
			}
		}
	}
	if h != hintNone {
		var match []object.ID
		for _, id := range ids {
			if r.fits(id, h) {
				match = append(match, id)
			}
		}
		if len(match) == 1 {
			return match[0], nil
		}
	}
	return object.ID{}, fmt.Errorf("%w: short object ID %s is ambiguous", ErrAmbiguous, prefix)
}

// fits reports whether id can serve where kind h is expected: commit-ish
// for commits, tree-ish for trees, the kind itself otherwise.
func (r *resolver) fits(id object.ID, h hint) bool {
	kind, _, err := r.src.Objects.Header(id)
	if err != nil {
		return false
	}
	if kind == object.Tag && h != hintTag {
		peeled, err := r.peelTags(id)
		if err != nil {
			return false
		}
		if kind, _, err = r.src.Objects.Header(peeled); err != nil {
			return false
		}
	}
	switch h {
	case hintCommit:
		return kind == object.Commit
	case hintTree:
		return kind == object.Tree || kind == object.Commit
	case hintBlob:
		return kind == object.Blob
	case hintTag:
		return kind == object.Tag
	}
	return true
}

// atSuffix evaluates <name>@{<inner>}.
func (r *resolver) atSuffix(name, inner string) (object.ID, error) {
	if name == "" && strings.HasPrefix(inner, "-") {
		n, err := strconv.Atoi(inner[1:])
		if err != nil || n <= 0 {
			return object.ID{}, unknown("bad @{-N} in %q", inner)
		}
		prev, err := r.previousCheckout(n)
		if err != nil {
			return object.ID{}, err
		}
		// The name is then taken as if typed: a tag of the same name wins,
		// as in git.
		return r.resolve(prev, hintNone)
	}
	if kind := branchSuffix(inner); kind != 0 {
		full, err := r.branchSuffixRef(name, kind)
		if err != nil {
			return object.ID{}, err
		}
		ref, err := r.src.Refs.Resolve(full)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return object.ID{}, unknown("%s does not exist", full)
			}
			return object.ID{}, err
		}
		return ref.ID, nil
	}
	if strings.HasPrefix(inner, "-") {
		return object.ID{}, unknown("@{-N} cannot follow a ref name")
	}
	full, err := r.reflogRef(name)
	if err != nil {
		return object.ID{}, err
	}
	// Numbers from 100000000 up are timestamps, not entry counts (observed).
	if n, err := strconv.Atoi(inner); err == nil && n >= 0 && n < 100000000 && !strings.HasPrefix(inner, "+") {
		return r.reflogNth(full, n)
	}
	return r.reflogAt(full, ParseDate(inner, time.Now()))
}

type suffixKind uint8

const (
	suffixUpstream suffixKind = iota + 1
	suffixPush
)

func branchSuffix(inner string) suffixKind {
	switch strings.ToLower(inner) {
	case "u", "upstream":
		return suffixUpstream
	case "push":
		return suffixPush
	}
	return 0
}

// prevSuffix parses a whole "@{-N}"; ok is false for anything else.
func prevSuffix(name string) (n int, ok bool, err error) {
	inner, found := strings.CutPrefix(name, "@{-")
	if !found || !strings.HasSuffix(inner, "}") {
		return 0, false, nil
	}
	n, err = strconv.Atoi(inner[:len(inner)-1])
	if err != nil || n <= 0 {
		return 0, true, unknown("bad @{-N} in %q", name)
	}
	return n, true, nil
}

// refName evaluates a name ending in @{upstream} or @{push} to the ref it
// denotes; other names are not refs here.
func (r *resolver) refName(name string) (string, error) {
	if strings.HasSuffix(name, "}") {
		if at := strings.LastIndex(name, "@{"); at >= 0 {
			if kind := branchSuffix(name[at+2 : len(name)-1]); kind != 0 {
				return r.branchSuffixRef(name[:at], kind)
			}
		}
	}
	return "", unknown("%q is not a ref name", name)
}

// reflogDWIM is the order in which git looks for the reflog of a short
// name: the first of these whose reflog exists (a tag without a reflog
// does not hide the branch's).
var reflogDWIM = [...]string{"%s", "refs/%s", "refs/tags/%s", "refs/heads/%s", "refs/remotes/%s", "refs/remotes/%s/HEAD"}

// reflogRef is the ref whose reflog <name>@{N} and <name>@{date} read: an
// empty name means the current branch (HEAD when detached).
func (r *resolver) reflogRef(name string) (string, error) {
	r.depth++
	defer func() { r.depth-- }()
	if r.depth > maxNesting {
		return "", unknown("expression nests too deeply")
	}
	switch {
	case name == "":
		target, err := r.src.Refs.Symbolic("HEAD")
		if err == nil {
			return target, nil
		}
		if errors.Is(err, refs.ErrNotSymbolic) {
			return "HEAD", nil
		}
		return "", err
	case name == "@":
		return "HEAD", nil
	}
	if n, ok, err := prevSuffix(name); ok {
		if err != nil {
			return "", err
		}
		prev, err := r.previousCheckout(n)
		if err != nil {
			return "", err
		}
		return r.reflogRef(prev)
	}
	if strings.Contains(name, "@{") {
		return r.refName(name)
	}
	for _, rule := range reflogDWIM {
		full := fmt.Sprintf(rule, name)
		if _, err := refs.CheckRefFormat(full, refs.FormatOptions{AllowOneLevel: true}); err != nil {
			continue
		}
		if _, err := r.src.Refs.Reflog(full); err == nil {
			return full, nil
		}
	}
	return "", unknown("no reflog for %q", name)
}

// branchSuffixRef evaluates <name>@{upstream} or <name>@{push} to the
// remote-tracking ref (or local branch) it denotes.
func (r *resolver) branchSuffixRef(name string, kind suffixKind) (string, error) {
	branch, err := r.branchOf(name)
	if err != nil {
		return "", err
	}
	tr := r.tracking()
	if kind == suffixUpstream {
		return tr.Upstream(branch)
	}
	return tr.Push(branch)
}

// branchOf returns the branch <name>@{upstream} applies to. As in git the
// name is a branch name taken literally, not looked up by the <refname>
// rules; "", "@" and "HEAD" mean the current branch, @{-N} the one
// checked out before.
func (r *resolver) branchOf(name string) (string, error) {
	if n, ok, err := prevSuffix(name); ok {
		if err != nil {
			return "", err
		}
		if name, err = r.previousCheckout(n); err != nil {
			return "", err
		}
	}
	if name == "" || name == "@" || name == "HEAD" {
		target, err := r.src.Refs.Symbolic("HEAD")
		if err != nil {
			if errors.Is(err, refs.ErrNotSymbolic) {
				return "", fmt.Errorf("%w: HEAD does not point to a branch", ErrNotBranch)
			}
			return "", err
		}
		branch, ok := strings.CutPrefix(target, "refs/heads/")
		if !ok {
			return "", fmt.Errorf("%w: HEAD does not point to a branch", ErrNotBranch)
		}
		return branch, nil
	}
	if err := refs.CheckBranchName(name); err != nil {
		return "", fmt.Errorf("%w: %q is not a branch name", ErrNotBranch, name)
	}
	return name, nil
}

func (r *resolver) tracking() *Tracking {
	return &Tracking{Config: r.src.Config}
}

// treePath looks up path in the tree-ish tree; "" is the tree itself.
func (r *resolver) treePath(treeish object.ID, revSpec, p string) (object.ID, error) {
	tree, err := r.peel(treeish, object.Tree, revSpec)
	if err != nil {
		return object.ID{}, err
	}
	clean, err := r.cleanPath(p)
	if err != nil {
		return object.ID{}, err
	}
	if clean == "" {
		return tree, nil
	}
	f := r.src.Objects.Format()
	cur := tree
	parts := strings.Split(clean, "/")
	for i, name := range parts {
		kind, data, err := r.src.Objects.Read(cur)
		if err != nil {
			return object.ID{}, r.missing(cur, err)
		}
		if kind != object.Tree {
			return object.ID{}, unknown("path '%s' does not exist in '%s'", clean, revSpec)
		}
		entries, err := object.ParseTree(f, data)
		if err != nil {
			return object.ID{}, err
		}
		found := false
		for _, e := range entries {
			if e.Name == name {
				cur, found = e.ID, true
				if i < len(parts)-1 && e.Mode != object.ModeTree {
					return object.ID{}, unknown("path '%s' does not exist in '%s'", clean, revSpec)
				}
				break
			}
		}
		if !found {
			return object.ID{}, unknown("path '%s' does not exist in '%s'", clean, revSpec)
		}
	}
	return cur, nil
}

// cleanPath makes a <path> relative to the top of the work tree: ./ and
// ../ forms are relative to the current directory (Options.Prefix).
func (r *resolver) cleanPath(p string) (string, error) {
	if p == "." || p == ".." || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") {
		joined := path.Join(r.opts.Prefix, p)
		if joined == ".." || strings.HasPrefix(joined, "../") {
			return "", unknown("'%s' is outside repository", p)
		}
		if joined == "." {
			joined = ""
		}
		return joined, nil
	}
	return strings.TrimRight(p, "/"), nil
}

// indexPath looks up path at stage in the index.
func (r *resolver) indexPath(p string, stage uint8) (object.ID, error) {
	clean, err := r.cleanPath(p)
	if err != nil {
		return object.ID{}, err
	}
	if r.src.Index == nil {
		return object.ID{}, unknown("no index")
	}
	idx, err := r.src.Index()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return object.ID{}, unknown("path '%s' is not in the index", clean)
		}
		return object.ID{}, err
	}
	pos, ok := idx.Find(clean, stage)
	if !ok {
		return object.ID{}, unknown("path '%s' is not in the index at stage %d", clean, stage)
	}
	return idx.Entries[pos].ID, nil
}

// compileSearch parses the text of :/<text> and ^{/<text>}: "!-" negates,
// "!!" escapes a literal '!', any other "!" prefix is reserved.
func compileSearch(text string) (*regexp.Regexp, bool, error) {
	negate := false
	if rest, ok := strings.CutPrefix(text, "!"); ok {
		switch {
		case strings.HasPrefix(rest, "-"):
			negate, text = true, rest[1:]
		case strings.HasPrefix(rest, "!"):
			text = rest
		default:
			return nil, false, fmt.Errorf("%w: reserved :/! modifier in %q", ErrUnsupported, text)
		}
	}
	// git uses POSIX extended regexps over the whole message, where '.'
	// also matches a newline and ^/$ anchor only at its ends. Go's RE2 is
	// close to ERE; it additionally knows Perl classes such as \s.
	re, err := regexp.Compile("(?s)" + text)
	if err != nil {
		return nil, false, unknown("bad regex %q: %v", text, err)
	}
	return re, negate, nil
}

// searchAll implements :/<text>: the youngest matching commit reachable
// from any ref or HEAD.
func (r *resolver) searchAll(text string) (object.ID, error) {
	re, negate, err := compileSearch(text)
	if err != nil {
		return object.ID{}, err
	}
	var starts []object.ID
	all, err := r.src.Refs.List("refs/")
	if err != nil {
		return object.ID{}, err
	}
	names := []string{"HEAD"}
	for _, ref := range all {
		names = append(names, ref.Name)
	}
	for _, name := range names {
		ref, err := r.src.Refs.Resolve(name)
		if err != nil {
			continue
		}
		if c, err := r.peelTags(ref.ID); err == nil {
			if kind, _, err := r.src.Objects.Header(c); err == nil && kind == object.Commit {
				starts = append(starts, c)
			}
		}
	}
	return r.search(starts, re, negate)
}

// search walks back from starts, newest commit first, for the first commit
// whose message matches re (or, negated, does not).
func (r *resolver) search(starts []object.ID, re *regexp.Regexp, negate bool) (object.ID, error) {
	id, ok, err := r.w.firstByDate(starts, func(c *commitInfo) (bool, error) {
		msg, err := r.w.message(c.id)
		if err != nil {
			return false, err
		}
		return re.MatchString(msg) != negate, nil
	})
	if err != nil {
		return object.ID{}, err
	}
	if !ok {
		return object.ID{}, unknown("no commit message matches %q", re)
	}
	return id, nil
}
