//go:build windows

package fetch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// setProcessGroup keeps exec's default cancellation, killing the child.
func setProcessGroup(cmd *exec.Cmd) {}

// findExecutable reports dir/name, trying the PATHEXT extensions when name
// has none, if it is a file.
func findExecutable(dir, name string) (string, bool) {
	exts := []string{""}
	if filepath.Ext(name) == "" {
		exts = strings.Split(strings.ToLower(os.Getenv("PATHEXT")), ";")
		if len(exts) == 1 && exts[0] == "" {
			exts = []string{".com", ".exe", ".bat", ".cmd"}
		}
	}
	for _, ext := range exts {
		p := filepath.Join(dir, name+ext)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}
