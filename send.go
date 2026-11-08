package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/internal/release"
	"github.com/stencil-hq/ovnfs/source"
)

// runSend attaches a folder or checkout to a machine reached by ssh, preparing
// that machine first: it installs ovnfs there from a release when it has none,
// attaches over ssh, and points PATH there at the attached checkout.
func runSend(c *cli, args []string) error {
	fs := c.flags("send")
	name := fs.String("name", "", "checkout name on the remote machine (default: the folder's name)")
	bin := fs.String("remote-bin", "", "path of ovnfs on the remote machine (default: on its PATH, else ~/.local/bin/ovnfs, installed there when missing)")
	pos, err := c.parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	target, dest := ".", pos[len(pos)-1]
	if len(pos) == 2 {
		target = pos[0]
	}
	host, at, _ := strings.Cut(dest, ":")
	if host == "" {
		return errors.New(c.usage())
	}
	a := api.AttachArgs{Name: *name}
	if a.Dir, a.Checkout, err = resolveTarget(target); err != nil {
		return err
	}
	if a.Dir != "" && a.Name == "" {
		root, err := source.Resolve(a.Dir)
		if err != nil {
			return err
		}
		a.Name = runLaneName(root)
	}
	r, err := probe(host, *bin, at)
	if err != nil {
		return err
	}
	if r.bin == "" {
		if err := r.install(c.ctx); err != nil {
			return err
		}
	}
	a.To, a.RemoteBin = host, r.bin
	reply, err := call[api.AttachReply](c, "attach", a)
	if err != nil {
		return err
	}
	where := reply.RemotePath
	if at != "" {
		if _, err := sshScript(host, nil, linkScript, at, reply.RemotePath); err != nil {
			return err
		}
		where = at
	}
	if !c.json {
		printAttached(host, where, reply)
	}
	return nil
}

// remote is a machine send prepares over ssh.
type remote struct {
	host            string // ssh destination
	kernel, machine string // uname -s and -m there
	home            string // $HOME there
	bin             string // ovnfs there; "" until installed
}

// probeScript refuses a link path $2 that exists as anything but a symlink, then
// prints the machine's kernel, architecture, home and ovnfs ($1, else on the
// PATH, else ~/.local/bin/ovnfs, else empty), one per line.
const probeScript = `[ -z "$2" ] || [ -L "$2" ] || [ ! -e "$2" ] || { echo "$2 exists and is not a symlink" >&2; exit 1; }; b=$1; [ -n "$b" ] || b=$(command -v ovnfs) || [ ! -x "$HOME/.local/bin/ovnfs" ] || b=$HOME/.local/bin/ovnfs; uname -s; uname -m; printf '%s\n' "$HOME" "$b"`

// probe asks host what send needs to know, refusing an unusable link path before
// anything is attached; bin names ovnfs there ("" to look for it) and at the
// path to link ("" for none).
func probe(host, bin, at string) (*remote, error) {
	out, err := sshScript(host, nil, probeScript, bin, at)
	if err != nil {
		return nil, err
	}
	f := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(f) != 4 {
		return nil, fmt.Errorf("%s answered the probe with %q", host, out)
	}
	return &remote{host: host, kernel: f[0], machine: f[1], home: f[2], bin: f[3]}, nil
}

// installScript stores the binary on stdin as $1/ovnfs once it runs there.
const installScript = `set -e; mkdir -p "$1"; t="$1/.ovnfs.$$"; trap 'rm -f "$t"' EXIT; cat >"$t"; chmod 755 "$t"; "$t" version >/dev/null; mv -f "$t" "$1/ovnfs"`

// install puts the release ovnfs into ~/.local/bin on the machine. The binary is
// downloaded here and streamed over ssh, so the machine needs no network access
// of its own.
func (r *remote) install(ctx context.Context) error {
	goos, goarch, err := platform(r.kernel, r.machine)
	if err != nil {
		return fmt.Errorf("%s: %w", r.host, err)
	}
	bin, err := release.Find(ctx, sendTag(), goos, goarch)
	if err != nil {
		return err
	}
	dir := r.home + "/.local/bin"
	fmt.Fprintf(os.Stderr, "installing ovnfs %s (%s/%s) on %s as %s/ovnfs\n", bin.Tag, goos, goarch, r.host, dir)
	exe, err := bin.Fetch(ctx)
	if err != nil {
		return err
	}
	if _, err := sshScript(r.host, bytes.NewReader(exe), installScript, dir); err != nil {
		return err
	}
	r.bin = dir + "/ovnfs"
	return nil
}

// platform maps uname's kernel and machine onto the GOOS and GOARCH of a release.
func platform(kernel, machine string) (goos, goarch string, err error) {
	switch kernel {
	case "Linux":
		goos = "linux"
	case "Darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("no ovnfs release for kernel %s; install ovnfs there and pass --remote-bin", kernel)
	}
	switch machine {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("no ovnfs release for machine %s; install ovnfs there and pass --remote-bin", machine)
	}
	return goos, goarch, nil
}

// sendTag is the release send installs: this binary's own, so both machines
// speak the same protocols, else "" (the latest) for a development build.
func sendTag() string {
	if bi, ok := debug.ReadBuildInfo(); ok && release.IsTag(bi.Main.Version) {
		return bi.Main.Version
	}
	return ""
}

// linkScript points $1 at $2, replacing a symlink there but nothing else.
const linkScript = `if [ -L "$1" ]; then rm -f "$1"; elif [ -e "$1" ]; then echo "$1 exists and is not a symlink" >&2; exit 1; fi; mkdir -p "$(dirname "$1")"; ln -s "$2" "$1"`

// sshScript runs a POSIX sh script on host with args as $1…, feeding it stdin,
// and returns what it printed. A leading ~/ in an argument expands to the home
// there, as scp does. Errors carry what ssh and the script printed on stderr.
func sshScript(host string, stdin io.Reader, script string, args ...string) ([]byte, error) {
	var b strings.Builder
	b.WriteString("sh -c ")
	b.WriteString(shellQuote(script))
	b.WriteString(" sh")
	for _, arg := range args {
		b.WriteByte(' ')
		if rest, ok := strings.CutPrefix(arg, "~/"); ok {
			b.WriteString(`"$HOME"/`)
			arg = rest
		}
		b.WriteString(shellQuote(arg))
	}
	cmd := sshCommand(host, b.String())
	cmd.Stdin = stdin
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := stderr.String(); msg != "" {
			return nil, fmt.Errorf("ssh %s: %s", host, msg)
		}
		return nil, fmt.Errorf("ssh %s: %w", host, err)
	}
	return out, nil
}

// sshCommand runs command through host's login shell, never prompting.
func sshCommand(host, command string) *exec.Cmd {
	return exec.Command("ssh", "-o", "BatchMode=yes", host, command)
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
