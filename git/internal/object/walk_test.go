package object

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestWalkMatchesLsTree(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			r := newRepo(t, "--object-format="+format)
			r.fastImport(history(r.format, 30, "walk"))
			db := r.open()
			for _, rev := range []string{"main", "main~3", "main~7", "v10"} {
				for _, opts := range []WalkOptions{{}, {Trees: true}} {
					args := []string{"ls-tree", "-r", "-l", "--full-tree", "-z"}
					if opts.Trees {
						args = append(args, "-t")
					}
					want := r.gitString(append(args, rev)...)
					var got strings.Builder
					for e, err := range db.Walk(r.id(r.gitString("rev-parse", rev)), opts) {
						if err != nil {
							t.Fatal(err)
						}
						size := "-"
						if e.Mode.Kind() == Blob {
							_, n, err := db.Header(e.ID)
							if err != nil {
								t.Fatal(err)
							}
							size = strconv.FormatInt(n, 10)
						}
						fmt.Fprintf(&got, "%s %s %s %7s\t%s\x00", e.Mode, e.Mode.Kind(), e.ID, size, e.Name)
					}
					if got.String() != want {
						t.Errorf("Walk(%s, %+v):\n%q\nwant\n%q", rev, opts, got.String(), want)
					}
				}
			}
		})
	}
}

func TestWalkStops(t *testing.T) {
	r := newRepo(t)
	r.fastImport(history(r.format, 3, "stop"))
	n := 0
	for range r.open().Walk(r.id(r.gitString("rev-parse", "main")), WalkOptions{}) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("iterated %d times after break", n)
	}
}

// diffTreeZ renders changes as git diff-tree -r --raw --no-renames -z does.
func diffTreeZ(changes []Change, f Format) string {
	var b strings.Builder
	for _, c := range changes {
		old, cur := c.Old.ID, c.New.ID
		if old.IsZero() {
			old = f.Zero()
		}
		if cur.IsZero() {
			cur = f.Zero()
		}
		fmt.Fprintf(&b, ":%s %s %s %s %c\x00%s\x00", c.Old.Mode, c.New.Mode, old, cur, c.Status, c.Path)
	}
	return b.String()
}

func TestDiffTreesMatchesDiffTree(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			r := newRepo(t, "--object-format="+format)
			r.fastImport(history(r.format, 30, "diff"))
			blob := func(s string) string {
				return strings.TrimSpace(string(r.git([]byte(s), "hash-object", "-w", "--stdin")))
			}
			mktree := func(lines ...string) string {
				return strings.TrimSpace(string(r.git([]byte(strings.Join(lines, "\n")+"\n"), "mktree")))
			}
			x, y := blob("x"), blob("y")
			link := r.id(r.gitString("rev-parse", "main")).String()
			sub := mktree("100644 blob "+x+"\tf", "100755 blob "+y+"\tg")
			deep := mktree("040000 tree "+sub+"\tinner", "100644 blob "+x+"\tz")
			t1 := mktree(
				"100644 blob "+x+"\ta",
				"100644 blob "+x+"\ta.b",
				"100644 blob "+x+"\ta-",
				"040000 tree "+sub+"\td",
				"040000 tree "+deep+"\tdeep",
				"100644 blob "+x+"\tmode",
				"100644 blob "+x+"\tfile2link",
				"120000 blob "+x+"\tlink2file",
				"160000 commit "+link+"\tmodule",
				"100644 blob "+x+"\tfile2module",
				"100644 blob "+x+"\tsame",
				"040000 tree "+sub+"\tgone",
			)
			t2 := mktree(
				"040000 tree "+sub+"\ta",
				"100644 blob "+y+"\ta.b",
				"100644 blob "+x+"\ta-",
				"100644 blob "+x+"\td",
				"040000 tree "+mktree("040000 tree "+sub+"\tinner", "100644 blob "+y+"\tz")+"\tdeep",
				"100755 blob "+x+"\tmode",
				"120000 blob "+x+"\tfile2link",
				"100644 blob "+x+"\tlink2file",
				"040000 tree "+sub+"\tmodule",
				"160000 commit "+link+"\tfile2module",
				"100644 blob "+x+"\tsame",
				"100644 blob "+y+"\tnew",
			)
			empty := r.format.Zero().String()
			pairs := [][2]string{{t1, t2}, {t2, t1}, {t1, t1}, {empty, t1}, {t2, empty}}
			for i := 1; i < 30; i += 4 {
				pairs = append(pairs, [2]string{fmt.Sprintf("main~%d", i), fmt.Sprintf("main~%d", i-1)})
				pairs = append(pairs, [2]string{"main", fmt.Sprintf("main~%d", i)})
			}
			db := r.open()
			emptyTree := strings.TrimSpace(string(r.git([]byte{}, "hash-object", "-t", "tree", "--stdin")))
			for _, p := range pairs {
				gitArgs := [2]string{p[0], p[1]}
				var ids [2]ID
				for k, rev := range p {
					if rev == empty {
						gitArgs[k] = emptyTree
						ids[k] = r.format.Zero()
						continue
					}
					ids[k] = r.id(r.gitString("rev-parse", rev))
				}
				want := r.gitString("diff-tree", "-r", "--raw", "--no-renames", "--no-abbrev", "-z", gitArgs[0], gitArgs[1])
				changes, err := db.DiffTrees(ids[0], ids[1])
				if err != nil {
					t.Fatal(err)
				}
				if got := diffTreeZ(changes, r.format); got != want {
					t.Errorf("DiffTrees(%s, %s):\n%q\nwant\n%q", p[0], p[1], got, want)
				}
			}
			// The two crafted trees must exercise every status.
			changes, _ := db.DiffTrees(r.id(t1), r.id(t2))
			for _, s := range []byte("ADMT") {
				if !bytes.Contains([]byte(diffTreeZ(changes, r.format)), []byte{' ', s, 0}) {
					t.Errorf("no %c change in the crafted diff", s)
				}
			}
		})
	}
}
