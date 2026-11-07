package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMarkCheckout rewrites the marker of a copied private git config as git would
// read it, keeping every other line, and leaves a correct marker file untouched.
func TestMarkCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	get := func(config string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"config", "--file", config}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git config %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	cases := map[string]string{
		"no section, no final newline": "[core]\n\tbare = false\n# trailing comment",
		"inherited marker":             "[core]\n\tbare = false\n[ovnfs]\n\tcheckout = parent\n[user]\n\tname = me\n",
		"inline and continued marker":  "[OVNFS] Checkout = parent\n[core]\n\tbare = false\n[ovnfs]\n\tcheckout = par\\\nent\n\tother = kept\n",
		"subsection kept":              "[ovnfs \"sub\"]\n\tcheckout = other\n[ovnfs]\n\t; checkout = commented\n\tcheckout = parent\n",
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			for _, marker := range []string{"child", `odd "name"; #x\`} {
				dir := t.TempDir()
				file := filepath.Join(dir, "config")
				if err := os.WriteFile(file, []byte(config), 0o640); err != nil {
					t.Fatal(err)
				}
				// Windows reports 0640 as 0666, so compare with what the host recorded.
				created, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				before := get(file, "--list")
				if err := MarkCheckout(dir, marker); err != nil {
					t.Fatal(err)
				}
				if got := get(file, "--get-all", "ovnfs.checkout"); got != marker {
					t.Fatalf("marker = %q, want %q", got, marker)
				}
				var kept []string
				for _, line := range strings.Split(before, "\n") {
					if !strings.HasPrefix(line, "ovnfs.checkout=") {
						kept = append(kept, line)
					}
				}
				after := get(file, "--list")
				for _, line := range kept {
					if !strings.Contains(after, line) {
						t.Fatalf("lost %q:\n%s", line, after)
					}
				}
				info, err := os.Stat(file)
				if err != nil || info.Mode().Perm() != created.Mode().Perm() {
					t.Fatalf("config mode %v, %v", info.Mode(), err)
				}
				old := time.Unix(1, 0)
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
				if err := MarkCheckout(dir, marker); err != nil {
					t.Fatal(err)
				}
				if info, err := os.Stat(file); err != nil || !info.ModTime().Equal(old) {
					t.Fatalf("marked config rewritten: %v", err)
				}
			}
		})
	}
}

// TestModuleDir places a submodule absorbed into its parent's modules directory, by
// a relative or an absolute gitfile, and no other nested repository.
func TestModuleDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "protocol.file.allow=always"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	work := t.TempDir()
	sub, app := filepath.Join(work, "sub"), filepath.Join(work, "app")
	for _, dir := range []string{sub, app} {
		git(work, "init", "-q", dir)
		git(dir, "commit", "-q", "--allow-empty", "-m", "init")
	}
	git(app, "submodule", "add", "-q", sub, "vendor/sub")
	git(app, "submodule", "add", "-q", sub, "vendor/abs")
	parent, err := Open(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "vendor", "abs", ".git"), []byte("gitdir: "+filepath.Join(parent.CommonDir, "modules", "vendor", "abs")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(app, "init", "-q", "own")
	git(work, "init", "-q", "--separate-git-dir", filepath.Join(work, "elsewhere.git"), filepath.Join(app, "separate"))
	for rel, want := range map[string]string{"vendor/sub": "modules/vendor/sub", "vendor/abs": "modules/vendor/abs", "own": "", "separate": ""} {
		repo, err := Open(filepath.Join(app, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := parent.ModuleDir(repo); got != want {
			t.Errorf("%s: ModuleDir = %q, want %q", rel, got, want)
		}
	}
}
