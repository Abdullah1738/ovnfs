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
				if err != nil || info.Mode().Perm() != 0o640 {
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
