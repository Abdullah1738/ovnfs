package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// gitProgram returns the git executable on PATH, if any. Fetches prefer it to
// package fetch so that a pack from an untrusted server is unpacked in git's
// process rather than in ovnfs.
func gitProgram() (string, bool) {
	program, err := exec.LookPath("git")
	return program, err == nil
}

// runGit runs the git executable program in r with args, leading `-c` pairs
// included. It never inherits another repository, index or work tree from the
// environment, runs no hooks, and adds env ("KEY=VALUE") to the environment. A
// failure carries git's stderr.
func (r *Repo) runGit(ctx context.Context, program string, env []string, args ...string) error {
	dir := r.Top
	if dir == "" {
		dir = r.GitDir // a bare repository
	}
	cmd := exec.CommandContext(ctx, program, append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "LC_ALL", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GIT_DIR", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(append(cmd.Env, "LC_ALL=C"), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		for len(args) >= 2 && args[0] == "-c" {
			args = args[2:]
		}
		return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return nil
}
