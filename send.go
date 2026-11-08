package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/source"
)

// releases is where send downloads ovnfs for machines that lack it.
const releases = "https://github.com/stencil-hq/ovnfs/releases"

// runSend attaches a folder or checkout to a machine reached by ssh, preparing
// that machine first: it installs ovnfs there from a GitHub release when it has
// none, joins it to this machine's mesh unless it is a member, attaches over the
// mesh, and points PATH there at the attached checkout.
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
	if a.To, err = r.join(c); err != nil {
		return err
	}
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
	host            string   // ssh destination
	kernel, machine string   // uname -s and -m there
	home            string   // $HOME there
	bin             string   // ovnfs there; "" until installed
	mesh            api.Mesh // its mesh status, read once bin runs
}

// probeScript refuses a link path $2 that exists as anything but a symlink, then
// prints the machine's kernel, architecture, home and ovnfs ($1, else on the
// PATH, else ~/.local/bin/ovnfs, else empty), then the mesh status that ovnfs
// reads, starting its daemon.
const probeScript = `[ -z "$2" ] || [ -L "$2" ] || [ ! -e "$2" ] || { echo "$2 exists and is not a symlink" >&2; exit 1; }; b=$1; [ -n "$b" ] || b=$(command -v ovnfs) || [ ! -x "$HOME/.local/bin/ovnfs" ] || b=$HOME/.local/bin/ovnfs; uname -s; uname -m; printf '%s\n' "$HOME" "$b"; [ -z "$b" ] || exec "$b" --json mesh ls`

// probe asks host what send needs to know, refusing an unusable link path before
// anything is attached; bin names ovnfs there ("" to look for it) and at the
// path to link ("" for none).
func probe(host, bin, at string) (*remote, error) {
	out, err := sshScript(host, nil, probeScript, bin, at)
	if err != nil {
		return nil, err
	}
	f := strings.SplitN(string(out), "\n", 5)
	if len(f) < 5 {
		return nil, fmt.Errorf("%s answered the probe with %q", host, out)
	}
	r := &remote{host: host, kernel: f[0], machine: f[1], home: f[2], bin: f[3]}
	if r.bin != "" {
		if err := json.Unmarshal([]byte(f[4]), &r.mesh); err != nil {
			return nil, fmt.Errorf("%s: reading the mesh status from %s: %w", host, r.bin, err)
		}
	}
	return r, nil
}

// installScript stores the binary on stdin as $1/ovnfs once it runs there, then
// prints the mesh status it reads, starting its daemon.
const installScript = `set -e; mkdir -p "$1"; t="$1/.ovnfs.$$"; trap 'rm -f "$t"' EXIT; cat >"$t"; chmod 755 "$t"; "$t" version >/dev/null; mv -f "$t" "$1/ovnfs"; exec "$1/ovnfs" --json mesh ls`

// install puts the release ovnfs into ~/.local/bin on the machine. The binary is
// downloaded here and streamed over ssh, so the machine needs no network access
// of its own.
func (r *remote) install(ctx context.Context) error {
	goos, goarch, err := platform(r.kernel, r.machine)
	if err != nil {
		return fmt.Errorf("%s: %w", r.host, err)
	}
	tag, err := releaseTag(ctx)
	if err != nil {
		return err
	}
	dir := r.home + "/.local/bin"
	fmt.Fprintf(os.Stderr, "installing ovnfs %s (%s/%s) on %s as %s/ovnfs\n", tag, goos, goarch, r.host, dir)
	exe, err := download(ctx, tag, goos, goarch)
	if err != nil {
		return err
	}
	out, err := sshScript(r.host, bytes.NewReader(exe), installScript, dir)
	if err != nil {
		return err
	}
	r.bin = dir + "/ovnfs"
	if err := json.Unmarshal(out, &r.mesh); err != nil {
		return fmt.Errorf("%s: reading the mesh status from %s: %w", r.host, r.bin, err)
	}
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

// releaseVersion matches a release tag, as opposed to a development build's
// pseudo-version or "(devel)".
var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// releaseTag picks the release to install: this binary's own, so both machines
// speak the same protocols, else (for a development build) the latest.
func releaseTag(ctx context.Context) (string, error) {
	if bi, ok := debug.ReadBuildInfo(); ok && releaseVersion.MatchString(bi.Main.Version) {
		return bi.Main.Version, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, releases+"/latest", nil)
	if err != nil {
		return "", err
	}
	// GitHub answers /releases/latest with a redirect to /releases/tag/TAG.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("finding the latest ovnfs release: %w", err)
	}
	resp.Body.Close()
	tag, ok := strings.CutPrefix(resp.Header.Get("Location"), releases+"/tag/")
	if !ok || tag == "" {
		return "", fmt.Errorf("finding the latest ovnfs release: %s/latest answered %s", releases, resp.Status)
	}
	return tag, nil
}

// download fetches the ovnfs binary out of a release's archive for one platform.
func download(ctx context.Context, tag, goos, goarch string) ([]byte, error) {
	url := fmt.Sprintf("%s/download/%s/ovnfs_%s_%s_%s.tar.gz", releases, tag, strings.TrimPrefix(tag, "v"), goos, goarch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s holds no ovnfs binary", url)
		}
		if err != nil {
			return nil, fmt.Errorf("downloading %s: %w", url, err)
		}
		if h.Name == "ovnfs" && h.Typeflag == tar.TypeReg {
			exe, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("downloading %s: %w", url, err)
			}
			return exe, nil
		}
	}
}

// join makes the machine a member of this machine's mesh, which mesh.setup
// starts when there is none, and returns the name attach reaches it by.
func (r *remote) join(c *cli) (string, error) {
	local, err := quietCall[api.Mesh](c, "mesh.ls", struct{}{})
	if err != nil {
		return "", err
	}
	if r.mesh.Node == local.Node {
		return "", fmt.Errorf("%s runs this machine's ovnfs node %s", r.host, abbrev(local.Node, 10))
	}
	member := slices.ContainsFunc(local.Members, func(m api.MeshMember) bool { return m.Node == r.mesh.Node })
	if !member || r.mesh.Mesh != local.Mesh {
		inv, err := quietCall[api.MeshInvite](c, "mesh.setup", api.MeshSetupArgs{})
		if err != nil {
			return "", err
		}
		out, err := sshScript(r.host, nil, `exec "$1" --json mesh join "$2"`, r.bin, inv.Token)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(out, &r.mesh); err != nil {
			return "", fmt.Errorf("%s: reading the mesh status from %s: %w", r.host, r.bin, err)
		}
		if local, err = quietCall[api.Mesh](c, "mesh.ls", struct{}{}); err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "joined %s to mesh %s\n", r.host, local.Mesh)
	}
	return memberName(local, r.mesh.Node)
}

// memberName is how attach --to reaches node in mesh m: by its name, unless
// another member shares it, else by its id.
func memberName(m api.Mesh, node string) (string, error) {
	i := slices.IndexFunc(m.Members, func(mem api.MeshMember) bool { return mem.Node == node })
	if i < 0 {
		return "", fmt.Errorf("node %s is not in mesh %s", abbrev(node, 10), m.Mesh)
	}
	name := m.Members[i].Name
	for _, mem := range m.Members {
		if mem.Node != node && !mem.Self && strings.EqualFold(mem.Name, name) {
			return node, nil
		}
	}
	return name, nil
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
