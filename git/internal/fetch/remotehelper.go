package fetch

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// helperCapabilities are the capabilities gitremote-helpers(7) defines; a
// helper marking any other mandatory ("*name") cannot be used.
var helperCapabilities = map[string]bool{
	"connect": true, "stateless-connect": true, "fetch": true, "import": true,
	"push": true, "export": true, "option": true, "refspec": true,
	"bidi-import": true, "export-marks": true, "import-marks": true,
	"signed-tags": true, "no-private-update": true, "object-format": true,
	"check-connectivity": true, "get": true,
}

// remoteHelper is a running git-remote-<name> program, driven with the
// commands of gitremote-helpers(7) over its stdin and stdout.
type remoteHelper struct {
	name string
	pc   *processConn
	out  *bufio.Reader // the helper's stdout, until a connection takes it over
	caps map[string]bool
}

// helper fetches through git-remote-<transport>: over the upload-pack
// stream of its connect or stateless-connect command when it offers one,
// else with its fetch command, which stores the objects in the target
// itself with all their history (Options.Depth is not passed on).
func (f *fetcher) helper(ctx context.Context, ep *endpoint) (Result, error) {
	if f.opts.GitDir == "" {
		return Result{}, fmt.Errorf("fetch: remote helper '%s' needs the target's git directory", ep.transport)
	}
	gitDir, err := filepath.Abs(f.opts.GitDir)
	if err != nil {
		return Result{}, err
	}
	h, err := startHelper(ctx, f.env, ep, gitDir)
	if err != nil {
		return Result{}, err
	}
	if h.caps["option"] {
		// Quiet, as git asks when its own output is not a terminal.
		for _, o := range [][2]string{{"progress", "false"}, {"verbosity", "0"}} {
			if _, err := h.option(o[0], o[1]); err != nil {
				return Result{}, h.pc.finish(err)
			}
		}
	}
	for _, command := range []string{"connect", "stateless-connect"} {
		if !h.caps[command] {
			continue
		}
		ok, err := h.connect(command)
		if err != nil {
			return Result{}, h.pc.finish(err)
		}
		if !ok {
			continue // the helper asked to fall back
		}
		h.pc.stdout = readCloser{h.out, h.pc.stdout}
		if command == "connect" {
			return f.stateful(h.pc)
		}
		return f.stateless(h.pc)
	}
	if !h.caps["fetch"] {
		return Result{}, h.pc.finish(fmt.Errorf("fetch: remote helper '%s' offers neither connect, stateless-connect nor fetch", h.name))
	}
	locks, err := h.fetch(f.req.format, f.opts.Wants)
	err = h.pc.finish(err)
	packDir := filepath.Join(gitDir, "objects", "pack")
	for _, l := range locks {
		// A .keep holding a received pack until refs are updated; only
		// those the protocol allows are removed.
		if filepath.Dir(l) == packDir && strings.HasSuffix(l, ".keep") {
			os.Remove(l)
		}
	}
	return Result{}, err
}

// readCloser reads through a buffered reader and closes the stream under it.
type readCloser struct {
	io.Reader
	io.Closer
}

// startHelper runs the helper git runs as "git remote-<ep.transport>" with
// the remote URL and its address as arguments, gitDir as GIT_DIR, and reads its
// capabilities.
func startHelper(ctx context.Context, env environ, ep *endpoint, gitDir string) (*remoteHelper, error) {
	argv, err := env.findProgram("remote-" + ep.transport)
	if err != nil {
		return nil, fmt.Errorf("fetch: remote helper '%s': %w", ep.transport, err)
	}
	cmd := command(ctx, argv[0], append(argv[1:], ep.remote, ep.path)...)
	cmd.Dir = gitDir
	cmd.Env = env.childEnv("GIT_DIR=" + gitDir)
	pc, err := startProcess(cmd)
	if err != nil {
		return nil, err
	}
	h := &remoteHelper{name: ep.transport, pc: pc, out: bufio.NewReader(pc.stdout), caps: map[string]bool{}}
	if err := h.send("capabilities"); err != nil {
		return nil, pc.finish(err)
	}
	for {
		line, err := h.readLine()
		if err != nil {
			return nil, pc.finish(err)
		}
		if line == "" {
			return h, nil
		}
		name, mandatory := strings.CutPrefix(line, "*")
		name, _, _ = strings.Cut(name, " ") // "refspec <refspec>" and the like
		h.caps[name] = true
		if mandatory && !helperCapabilities[name] {
			return nil, pc.finish(fmt.Errorf("fetch: remote helper '%s' requires unknown capability '%s'", h.name, name))
		}
	}
}

func (h *remoteHelper) send(line string) error {
	if _, err := io.WriteString(h.pc.stdin, line+"\n"); err != nil {
		return fmt.Errorf("fetch: remote helper '%s': %w", h.name, err)
	}
	return nil
}

// readLine returns the helper's next output line without its newline.
func (h *remoteHelper) readLine() (string, error) {
	line, err := h.out.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			return "", fmt.Errorf("fetch: remote helper '%s' exited unexpectedly", h.name)
		}
		return "", fmt.Errorf("fetch: remote helper '%s': %w", h.name, err)
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// option sets a helper option, reporting whether the helper accepted it.
func (h *remoteHelper) option(name, value string) (bool, error) {
	if err := h.send("option " + name + " " + value); err != nil {
		return false, err
	}
	reply, err := h.readLine()
	return reply == "ok", err
}

// connect asks for a connection to git-upload-pack with command ("connect"
// or "stateless-connect"); ok is false when the helper asks to fall back to
// its other capabilities.
func (h *remoteHelper) connect(command string) (ok bool, err error) {
	if err := h.send(command + " git-upload-pack"); err != nil {
		return false, err
	}
	reply, err := h.readLine()
	switch {
	case err != nil:
		return false, err
	case reply == "":
		return true, nil
	case reply == "fallback":
		return false, nil
	}
	return false, fmt.Errorf("fetch: remote helper '%s': unexpected reply %q to %s", h.name, reply, command)
}

// fetch has the helper store wants in the target with its list and fetch
// commands, and returns the .keep files it reports. Wants are named after a
// listed ref that points at them, else by their id, as git names them.
func (h *remoteHelper) fetch(format object.Format, wants []object.ID) (locks []string, err error) {
	if h.caps["option"] && h.caps["object-format"] {
		if _, err := h.option("object-format", "true"); err != nil {
			return nil, err
		}
	}
	if err := h.send("list"); err != nil {
		return nil, err
	}
	want := idSet(wants)
	names := make(map[object.ID]string, len(wants))
	for {
		line, err := h.readLine()
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		if keyword, ok := strings.CutPrefix(line, ":"); ok {
			if algo, ok := strings.CutPrefix(keyword, "object-format "); ok && algo != format.String() {
				return nil, fmt.Errorf("fetch: the remote repository uses %s object names, the target uses %s", algo, format)
			}
			continue
		}
		value, rest, _ := strings.Cut(line, " ")
		name, _, _ := strings.Cut(rest, " ")
		if id, err := object.ParseIDFormat(format, value); err == nil {
			if _, ok := want[id]; ok && names[id] == "" {
				names[id] = name
			}
		}
	}
	var batch strings.Builder
	for _, id := range wants {
		name := names[id]
		if name == "" {
			name = id.String()
		}
		fmt.Fprintf(&batch, "fetch %s %s\n", id, name)
	}
	if err := h.send(batch.String()); err != nil {
		return nil, err
	}
	for {
		line, err := h.readLine()
		if err != nil {
			return locks, err
		}
		if line == "" {
			break
		}
		if file, ok := strings.CutPrefix(line, "lock "); ok {
			locks = append(locks, file)
		} else if line != "connectivity-ok" {
			return locks, fmt.Errorf("fetch: remote helper '%s': unexpected reply %q to fetch", h.name, line)
		}
	}
	// A blank line ends the command stream.
	return locks, h.send("")
}
