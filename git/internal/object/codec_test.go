package object

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func TestEncodeTreeMatchesMktree(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			r := newRepo(t, "--object-format="+format)
			blob := Hash(r.format, Blob, []byte("x"))
			tree := Hash(r.format, Tree, nil)
			var entries []TreeEntry
			var lines []string
			for _, name := range []string{"a", "a.b", "a-", "a0", "ab", "b", "z", "Z", "~", "é"} {
				for i, mode := range []Mode{ModeFile, ModeTree, ModeExec, ModeSymlink, ModeGitlink} {
					n := name + strings.Repeat("_", i)
					if i == 1 {
						n = name + "dir"
					}
					id := blob
					switch mode {
					case ModeTree:
						id = tree
					case ModeGitlink:
						id = Hash(r.format, Commit, []byte(n))
					}
					entries = append(entries, TreeEntry{Name: n, Mode: mode, ID: id})
					lines = append(lines, fmt.Sprintf("%s %s %s\t%s", mode, mode.Kind(), id, n))
				}
			}
			// Names equal but for a trailing '/' in tree order.
			entries = append(entries, TreeEntry{Name: "q", Mode: ModeTree, ID: tree}, TreeEntry{Name: "q.x", Mode: ModeFile, ID: blob})
			lines = append(lines, fmt.Sprintf("040000 tree %s\tq", tree), fmt.Sprintf("100644 blob %s\tq.x", blob))
			rand.New(rand.NewPCG(1, 2)).Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
			want := r.id(string(r.git([]byte(strings.Join(lines, "\n")+"\n"), "mktree", "--missing")))
			before := slices.Clone(entries)
			data := EncodeTree(entries)
			if got := Hash(r.format, Tree, data); got != want {
				t.Errorf("EncodeTree hash %s, want %s", got, want)
			}
			if !slices.Equal(entries, before) {
				t.Error("EncodeTree reordered its argument")
			}
			parsed, err := ParseTree(r.format, data)
			if err != nil || !bytes.Equal(EncodeTree(parsed), data) {
				t.Errorf("ParseTree round trip failed: %v", err)
			}
		})
	}
}

func TestParseTreeRoundTripsGitTrees(t *testing.T) {
	r := newRepo(t)
	r.fastImport(history(r.format, 20, "trees"))
	n := 0
	for id, o := range r.allObjects() {
		if o.kind != Tree {
			continue
		}
		n++
		entries, err := ParseTree(r.format, o.data)
		if err != nil {
			t.Fatalf("tree %s: %v", id, err)
		}
		if !bytes.Equal(EncodeTree(entries), o.data) {
			t.Errorf("tree %s does not round-trip", id)
		}
	}
	if n == 0 {
		t.Fatal("no trees")
	}
}

func TestParseTreeModesAndErrors(t *testing.T) {
	raw := bytes.Repeat([]byte{0xab}, SHA1.Size())
	entry := func(mode, name string) []byte {
		return append([]byte(mode+" "+name+"\x00"), raw...)
	}
	// Canonical modes as git ls-tree reports them for odd stored modes.
	for stored, want := range map[string]Mode{
		"100644": ModeFile, "100664": ModeFile, "100000": ModeFile, "100700": ModeExec, "100100": ModeExec,
		"40000": ModeTree, "040000": ModeTree, "120777": ModeSymlink, "160000": ModeGitlink,
		"60000": ModeGitlink, "170000": ModeGitlink, "0": ModeGitlink,
	} {
		e, err := ParseTree(SHA1, entry(stored, "n"))
		if err != nil || len(e) != 1 || e[0].Mode != want {
			t.Errorf("mode %s parsed as %v %v, want %v", stored, e, err, want)
		}
	}
	for _, bad := range [][]byte{
		[]byte("100644 name"),
		entry("100648", "n"),
		entry("", "n"),
		entry("100644", ""),
		entry("100644", "a/b"),
		entry("100644", "n")[:20],
	} {
		if _, err := ParseTree(SHA1, bad); err == nil {
			t.Errorf("ParseTree(%q) succeeded", bad)
		}
	}
}

func TestParseCommitAgainstGit(t *testing.T) {
	r := newRepo(t)
	tree := r.git([]byte{}, "mktree")
	p1 := r.id(r.gitString("commit-tree", "-m", "p1", strings.TrimSpace(string(tree))))
	p2 := r.id(r.gitString("commit-tree", "-m", "p2", strings.TrimSpace(string(tree))))
	raw := fmt.Sprintf("tree %s\nparent %s\nparent %s\n"+
		"author Ann Author <ann@example.com> 1700000000 +0530\n"+
		"committer Cid <cid@example.com> 1700000123 -0700\n"+
		"encoding ISO-8859-1\n"+
		"mergetag object %s\n type commit\n tag t\n tagger T <t@example.com> 1 +0000\n \n msg\n"+
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n"+
		"\nsubject\n\nbody\n", strings.TrimSpace(string(tree)), p1, p2, p2)
	id := r.id(string(r.git([]byte(raw), "hash-object", "-w", "-t", "commit", "--stdin")))
	c, err := ParseCommit(r.format, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Split(r.gitString("log", "-1", "--format=%T%n%P%n%an%n%ae%n%at%n%ai%n%cn%n%ce%n%ct%n%ci%n%e", id.String()), "\n")
	got := []string{
		c.Tree.String(), c.Parents[0].String() + " " + c.Parents[1].String(),
		c.Author.Name, c.Author.Email, fmt.Sprint(c.Author.When.Unix()), c.Author.When.Format("2006-01-02 15:04:05 -0700"),
		c.Committer.Name, c.Committer.Email, fmt.Sprint(c.Committer.When.Unix()), c.Committer.When.Format("2006-01-02 15:04:05 -0700"),
		c.Encoding,
	}
	if !slices.Equal(got, f[:len(got)]) {
		t.Errorf("parsed %q\ngit    %q", got, f[:len(got)])
	}
	if c.Message != "subject\n\nbody\n" {
		t.Errorf("message %q", c.Message)
	}
	wantExtra := []ExtraHeader{
		{"mergetag", "object " + p2.String() + "\ntype commit\ntag t\ntagger T <t@example.com> 1 +0000\n\nmsg"},
		{"gpgsig", "-----BEGIN PGP SIGNATURE-----\n\nabc\n-----END PGP SIGNATURE-----"},
	}
	if !slices.Equal(c.Extra, wantExtra) {
		t.Errorf("extra headers %q, want %q", c.Extra, wantExtra)
	}
	if !strings.HasSuffix(r.gitString("cat-file", "-p", id.String()), "\nsubject\n\nbody\n") {
		t.Error("git disagrees about the message")
	}

	// A malformed date keeps name and email.
	c, err = ParseCommit(SHA1, []byte("tree "+strings.Repeat("0", 40)+"\nauthor N <e@x> garbage\n\nm"))
	if err != nil || c.Author.Name != "N" || c.Author.Email != "e@x" || !c.Author.When.IsZero() {
		t.Errorf("lenient author = %+v %v", c.Author, err)
	}
	for _, bad := range []string{"parent " + strings.Repeat("0", 40) + "\n\nm", "tree xyz\n\nm"} {
		if _, err := ParseCommit(SHA1, []byte(bad)); err == nil {
			t.Errorf("ParseCommit(%q) succeeded", bad)
		}
	}
}

func TestParseTagAgainstGit(t *testing.T) {
	r := newRepo(t, "--object-format=sha256")
	r.fastImport(history(r.format, 10, "tag"))
	r.git(nil, "tag", "-a", "-m", "nested\n\nbody", "nested", "v10")
	_, data, err := r.open().Read(r.id(r.gitString("rev-parse", "nested")))
	if err != nil {
		t.Fatal(err)
	}
	tag, err := ParseTag(r.format, data)
	if err != nil {
		t.Fatal(err)
	}
	want := r.gitString("for-each-ref", "--format=%(object) %(type) %(tag) %(taggername) %(taggeremail) %(taggerdate:raw)", "refs/tags/nested")
	got := fmt.Sprintf("%s %s %s %s <%s> %s\n", tag.Object, tag.Type, tag.Name, tag.Tagger.Name, tag.Tagger.Email,
		tag.Tagger.String()[strings.LastIndexByte(tag.Tagger.String(), '>')+2:])
	if got != want {
		t.Errorf("parsed %q, git %q", got, want)
	}
	if tag.Message != "nested\n\nbody\n" {
		t.Errorf("message %q", tag.Message)
	}
	if _, err := ParseTag(SHA1, []byte("tag x\n\nm")); err == nil {
		t.Error("tag without object parsed")
	}
}
