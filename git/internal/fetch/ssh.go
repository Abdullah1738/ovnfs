package fetch

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
)

// sshVariant is the command-line dialect of the ssh program (ssh.variant).
type sshVariant uint8

const (
	variantAuto sshVariant = iota
	variantSSH
	variantSimple
	variantPlink
	variantPutty
	variantTortoise
)

func parseVariant(s string) sshVariant {
	switch strings.ToLower(s) {
	case "auto":
		return variantAuto
	case "simple":
		return variantSimple
	case "plink":
		return variantPlink
	case "putty":
		return variantPutty
	case "tortoiseplink":
		return variantTortoise
	}
	// config/ssh.adoc: any other value is treated as ssh.
	return variantSSH
}

// sshProgram is the configured ssh command.
type sshProgram struct {
	// shell is a command line run through the shell (GIT_SSH_COMMAND,
	// core.sshCommand); otherwise prog is a program path (GIT_SSH, "ssh").
	shell   string
	prog    string
	variant sshVariant
}

// resolveSSH picks the ssh command (GIT_SSH_COMMAND, GIT_SSH,
// core.sshCommand, then "ssh") and its variant (GIT_SSH_VARIANT, ssh.variant,
// then the command's basename; unknown names are auto-detected later).
func resolveSSH(cfg *config.Config, env environ) sshProgram {
	var p sshProgram
	var core string
	if cfg != nil {
		core, _ = cfg.Get("core.sshcommand")
	}
	switch {
	case env.get("GIT_SSH_COMMAND") != "":
		p.shell = env.get("GIT_SSH_COMMAND")
	case env.get("GIT_SSH") != "":
		p.prog = env.get("GIT_SSH")
	case core != "":
		p.shell = core
	default:
		p.prog = "ssh"
	}
	if v := env.get("GIT_SSH_VARIANT"); v != "" {
		p.variant = parseVariant(v)
		return p
	}
	if cfg != nil {
		if v, ok := cfg.Get("ssh.variant"); ok {
			p.variant = parseVariant(v)
			return p
		}
	}
	name := p.prog
	if p.shell != "" {
		name = firstWord(p.shell)
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(name, `\`, "/")))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "ssh":
		p.variant = variantSSH
	case "plink":
		p.variant = variantPlink
	case "tortoiseplink":
		p.variant = variantTortoise
	default:
		p.variant = variantAuto
	}
	return p
}

// firstWord returns the program word of a shell command line, honouring
// simple single and double quoting.
func firstWord(s string) string {
	s = strings.TrimLeft(s, " \t")
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote != '\'' && c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case quote == 0 && (c == ' ' || c == '\t'):
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// cmd builds the command for args.
func (p sshProgram) cmd(ctx context.Context, env environ, args ...string) (*exec.Cmd, error) {
	if p.shell != "" {
		return env.shellCommand(ctx, p.shell, args...)
	}
	return command(ctx, p.prog, args...), nil
}

// args returns the arguments before the remote command for variant v, and
// whether v passes GIT_PROTOCOL (only OpenSSH, through SendEnv).
func sshArgs(v sshVariant, ep *endpoint) ([]string, bool, error) {
	dest := ep.host
	if ep.user != "" {
		dest = ep.user + "@" + ep.host
	}
	var args []string
	switch v {
	case variantSSH:
		args = append(args, "-o", "SendEnv=GIT_PROTOCOL")
		if ep.port != "" {
			args = append(args, "-p", ep.port)
		}
		return append(args, dest), true, nil
	case variantSimple:
		if ep.port != "" {
			return nil, false, fmt.Errorf("fetch: ssh variant 'simple' does not support setting port")
		}
		return []string{dest}, false, nil
	case variantTortoise:
		args = append(args, "-batch")
		fallthrough
	case variantPlink, variantPutty:
		if ep.port != "" {
			args = append(args, "-P", ep.port)
		}
		return append(args, dest), false, nil
	}
	panic("fetch: unresolved ssh variant")
}

// detectVariant resolves "auto" the way config/ssh.adoc describes: OpenSSH
// if the command accepts -G (print configuration), else simple.
func (p sshProgram) detectVariant(ctx context.Context, env environ, ep *endpoint) sshVariant {
	args, _, _ := sshArgs(variantSSH, ep)
	cmd, err := p.cmd(ctx, env, append([]string{"-G"}, args...)...)
	if err != nil {
		return variantSimple
	}
	cmd.Env = env.childEnv("GIT_PROTOCOL=version=2")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() == nil {
		return variantSSH
	}
	return variantSimple
}

// dialSSH starts git-upload-pack on the remote host through ssh.
func dialSSH(ctx context.Context, cfg *config.Config, env environ, ep *endpoint) (*processConn, error) {
	p := resolveSSH(cfg, env)
	v := p.variant
	if v == variantAuto {
		v = p.detectVariant(ctx, env, ep)
	}
	args, sendsEnv, err := sshArgs(v, ep)
	if err != nil {
		return nil, err
	}
	args = append(args, "git-upload-pack "+shellQuote(ep.path))
	cmd, err := p.cmd(ctx, env, args...)
	if err != nil {
		return nil, err
	}
	if sendsEnv {
		cmd.Env = env.childEnv("GIT_PROTOCOL=version=2")
	} else {
		cmd.Env = env.childEnv()
	}
	return startProcess(cmd)
}

// processConn talks to a child process over its stdin and stdout.
type processConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *stderrBuffer
}

func startProcess(cmd *exec.Cmd) (*processConn, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	pc := &processConn{cmd: cmd, stdin: stdin, stdout: stdout, stderr: &stderrBuffer{}}
	cmd.Stderr = pc.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fetch: starting %s: %w", cmd.Path, err)
	}
	return pc, nil
}

func (pc *processConn) Read(b []byte) (int, error)  { return pc.stdout.Read(b) }
func (pc *processConn) Write(b []byte) (int, error) { return pc.stdin.Write(b) }

// finish ends the conversation, closing stdin so upload-pack exits, and
// waits for the process. A conversation that failed gets the child's exit
// status and stderr added; one that succeeded ignores how the child exits,
// since the pack has arrived.
func (pc *processConn) finish(err error) error {
	pc.stdin.Close()
	if err != nil {
		// Unblock a child still writing output nobody will read.
		pc.stdout.Close()
	}
	werr := pc.cmd.Wait()
	if err == nil {
		return nil
	}
	if werr != nil {
		err = fmt.Errorf("%w (%v)", err, werr)
	}
	return withStderr(err, pc.stderr)
}
