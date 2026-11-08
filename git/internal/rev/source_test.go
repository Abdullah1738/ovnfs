package rev

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/refs"
)

// replaceObjects applies refs/replace/* to reads, standing in for the
// repository package's replace-aware reader.
type replaceObjects struct {
	*object.DB
	replace map[object.ID]object.ID
}

func (o *replaceObjects) target(id object.ID) object.ID {
	for range 10 {
		r, ok := o.replace[id]
		if !ok {
			break
		}
		id = r
	}
	return id
}

func (o *replaceObjects) Read(id object.ID) (object.Kind, []byte, error) {
	return o.DB.Read(o.target(id))
}

func (o *replaceObjects) Header(id object.ID) (object.Kind, int64, error) {
	return o.DB.Header(o.target(id))
}

// openSource builds a Source for the repository git reports for dir.
func openSource(t *testing.T, g *gitRunner, dir string) *Source {
	t.Helper()
	gitDir := g.run(dir, "rev-parse", "--path-format=absolute", "--git-dir")
	commonDir := g.run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	workTree := ""
	if g.run(dir, "rev-parse", "--is-bare-repository") == "false" {
		workTree = g.run(dir, "rev-parse", "--show-toplevel")
	}
	envMap := map[string]string{}
	for _, kv := range g.env {
		k, v, _ := strings.Cut(kv, "=")
		envMap[k] = v
	}
	lookup := func(k string) (string, bool) { v, ok := envMap[k]; return v, ok }
	refStore := func(f object.Format, backend refs.Backend) *refs.Store {
		s, err := refs.Open(refs.Options{GitDir: gitDir, CommonDir: commonDir, Format: f, Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cfg, err := config.Load(config.Options{
		GitDir: gitDir, CommonDir: commonDir, WorkTree: workTree, LookupEnv: lookup,
		Branch: func() (string, error) {
			s, err := refStore(object.SHA1, "").Symbolic("HEAD")
			if err != nil {
				return "", nil
			}
			return s, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fname, _ := cfg.Get("extensions.objectformat")
	f, err := object.ParseFormat(strings.ToLower(fname))
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := cfg.Get("extensions.refstorage")
	rs := refStore(f, refs.Backend(strings.ToLower(backend)))
	db, err := object.Open(filepath.Join(commonDir, "objects"), f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	src := &Source{Refs: rs, Objects: db, Config: cfg}
	replaced, err := rs.List("refs/replace/")
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) > 0 {
		ro := &replaceObjects{DB: db, replace: map[object.ID]object.ID{}}
		for _, r := range replaced {
			from, err := object.ParseIDFormat(f, strings.TrimPrefix(r.Name, "refs/replace/"))
			if err != nil {
				continue
			}
			to, err := rs.Resolve(r.Name)
			if err != nil {
				t.Fatal(err)
			}
			ro.replace[from] = to.ID
		}
		src.Objects, src.Replaced = ro, true
	}
	if file, err := os.Open(filepath.Join(commonDir, "shallow")); err == nil {
		src.Shallow = map[object.ID]bool{}
		sc := bufio.NewScanner(file)
		for sc.Scan() {
			id, err := object.ParseIDFormat(f, strings.TrimSpace(sc.Text()))
			if err != nil {
				t.Fatal(err)
			}
			src.Shallow[id] = true
		}
		file.Close()
	}
	src.Index = func() (*index.Index, error) {
		return index.Read(filepath.Join(gitDir, "index"), f, index.ReadOptions{})
	}
	return src
}

// fastImport builds history with git fast-import from a script whose
// "commit" blocks are written by the caller.
// It returns the object name of each mark.
func fastImport(t *testing.T, g *gitRunner, dir, script string) map[int]string {
	t.Helper()
	marks := filepath.Join(t.TempDir(), "marks")
	g.runInput(dir, script+"done\n", "fast-import", "--quiet", "--done", "--force", "--export-marks="+marks)
	data, err := os.ReadFile(marks)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var mark int
		var name string
		if _, err := fmt.Sscanf(line, ":%d %s", &mark, &name); err != nil {
			t.Fatalf("marks line %q: %v", line, err)
		}
		out[mark] = name
	}
	return out
}

// importCommit renders one fast-import commit block.
func importCommit(ref string, mark int, when int64, msg string, parents []int, files map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "commit %s\nmark :%d\ncommitter C O Mitter <c@example.com> %d +0000\n", ref, mark, when)
	fmt.Fprintf(&b, "data %d\n%s\n", len(msg), msg)
	for i, p := range parents {
		if i == 0 {
			fmt.Fprintf(&b, "from :%d\n", p)
		} else {
			fmt.Fprintf(&b, "merge :%d\n", p)
		}
	}
	for name, content := range files {
		fmt.Fprintf(&b, "M 100644 inline %s\ndata %d\n%s\n", name, len(content), content)
	}
	b.WriteString("\n")
	return b.String()
}
