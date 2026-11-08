package fetch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// environ answers environment lookups through the caller's function and
// builds child process environments consistent with it.
type environ struct {
	lookup func(string) (string, bool)
}

func (e environ) get(key string) string {
	v, _ := e.lookup(key)
	return v
}

// ignoredVars are never passed to children: ovnfs behaves as if they were
// unset, and a helper or ssh wrapper running git must not see them either.
var ignoredVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_PROTOCOL", "GIT_TERMINAL_PROMPT", "GCM_INTERACTIVE",
}

// lookedUpVars are the variables this package or its children consult;
// children get their values from the lookup function rather than from the
// process environment, so overrides the caller makes reach them.
var lookedUpVars = []string{
	"PATH", "HOME", "XDG_CONFIG_HOME", "GIT_EXEC_PATH",
	"GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT",
	"GIT_ASKPASS", "SSH_ASKPASS",
	"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL",
}

// childEnv is the process environment with lookedUpVars taken from the
// lookup function, ignoredVars removed, prompting disabled, and extra
// ("KEY=VALUE") appended.
func (e environ) childEnv(extra ...string) []string {
	drop := func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		for _, list := range [][]string{ignoredVars, lookedUpVars} {
			for _, v := range list {
				if k == v || runtime.GOOS == "windows" && strings.EqualFold(k, v) {
					return true
				}
			}
		}
		return false
	}
	var env []string
	for _, kv := range os.Environ() {
		if !drop(kv) {
			env = append(env, kv)
		}
	}
	for _, k := range lookedUpVars {
		if v, ok := e.lookup(k); ok {
			env = append(env, k+"="+v)
		}
	}
	// Never prompt: ovnfs has no terminal to offer, and Git Credential
	// Manager must not pop up its UI either.
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	return append(env, extra...)
}

// lookPath finds an executable in the PATH the lookup function reports.
func (e environ) lookPath(name string) (string, error) {
	for dir := range strings.SplitSeq(e.get("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			dir = "."
		}
		if p, ok := findExecutable(dir, name); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}

// shellCommand runs script through the shell with args available as "$@",
// as git runs GIT_SSH_COMMAND, core.sshCommand and credential helpers.
// Without a shell (Windows lacking Git for Windows) a script free of shell
// syntax is split on whitespace and run directly; any other is an error.
func (e environ) shellCommand(ctx context.Context, script string, args ...string) (*exec.Cmd, error) {
	if sh, err := e.lookPath("sh"); err == nil {
		return command(ctx, sh, append([]string{"-c", script + ` "$@"`, script}, args...)...), nil
	}
	words, ok := plainWords(script)
	if !ok {
		return nil, fmt.Errorf("fetch: running %q needs a shell, but sh was not found in PATH", script)
	}
	return command(ctx, words[0], append(words[1:], args...)...), nil
}

// plainWords splits a command line that has no shell syntax, where the
// shell would only split words; ok is false for anything else.
func plainWords(script string) (words []string, ok bool) {
	if strings.ContainsAny(script, "'\"$`|&;<>()*?[]~\n\\") {
		return nil, false
	}
	words = strings.Fields(script)
	return words, len(words) > 0
}

// command runs a program directly.
func command(ctx context.Context, prog string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, prog, args...)
	prepareCmd(cmd)
	return cmd
}

// prepareCmd makes cancellation kill the child and everything it started,
// and bounds how long Wait waits for pipes a grandchild keeps open.
func prepareCmd(cmd *exec.Cmd) {
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
}

// stderrBuffer keeps the first 64 KiB a child writes to stderr, for errors.
type stderrBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *stderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := 64<<10 - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *stderrBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}

// withStderr adds a child's captured stderr to err.
func withStderr(err error, stderr *stderrBuffer) error {
	if err == nil {
		return nil
	}
	if s := stderr.String(); s != "" {
		return fmt.Errorf("%w\n%s", err, s)
	}
	return err
}
