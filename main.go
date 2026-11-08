// Command ovnfs opens copy-on-write checkouts of any folder and serves them to agents.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"syscall"
	"text/tabwriter"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/manifest"
)

// defaultState and defaultMount are where the daemon keeps its state and shows
// checkouts unless flags, the environment or ~/.o2/ovnfs.yml say otherwise (see
// resolveDirs). On Linux and macOS both live under the user's home: on Linux the
// only place distribution FUSE confinement (Ubuntu's fusermount3 AppArmor
// profile) lets a mount go, on macOS the volume that usually holds the
// repositories, so pins clone rather than copy. On Windows state is in
// %LOCALAPPDATA% and checkouts on a free drive letter, where the daemon maps the
// SMB share and WinFsp mounts its drive.
var defaultState, defaultMount = defaultDirs()

// defaultDirs picks the per-OS defaults.
func defaultDirs() (string, string) {
	switch runtime.GOOS {
	case "windows":
		local, err := os.UserCacheDir()
		if err != nil {
			local = os.TempDir()
		}
		return filepath.Join(local, "ovnfs"), `O:\`
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	state := filepath.Join(home, ".local", "state", "ovnfs")
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		state = filepath.Join(xdg, "ovnfs")
	}
	return state, filepath.Join(home, "checkouts")
}

// exitCode ends the process with a status after the command already reported why.
type exitCode int

// Error describes the silent exit status for callers that inspect it.
func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// main dispatches one verb and maps its error onto the exit status.
func main() {
	err := run(os.Args[1:])
	var code exitCode
	switch {
	case err == nil:
	case errors.As(err, &code):
		os.Exit(int(code))
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// meshRemoved answers the removed `ovnfs mesh` verb, pointing at the ssh links
// that replaced it; it stays out of help.
const meshRemoved = `ovnfs mesh is deprecated and was removed: machines now connect over ssh, with no setup or join step.
  ovnfs attach DIR --to HOST        make DIR appear on HOST; content moves on first read
  ovnfs send DIR HOST[:PATH]        the same, installing ovnfs on HOST first if missing; PATH there links to it
  ovnfs push [CHECKOUT]             have attached machines fetch everything, before this one goes offline
HOST is any ssh destination (~/.ssh/config, ProxyJump and the like apply); it must accept a key without prompting.
The old mesh state in <state>/mesh can be deleted.`

// run resolves global options, then hands the verb its remaining arguments.
func run(args []string) error {
	c := &cli{}
	fs := c.flags("ovnfs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			return nil
		}
		return fmt.Errorf("%w; run 'ovnfs help'", err)
	}
	if fs.NArg() == 0 {
		printUsage(os.Stdout)
		return nil
	}
	verb, rest := fs.Arg(0), fs.Args()[1:]
	switch verb {
	case "help":
		printUsage(os.Stdout)
		return nil
	case "version":
		// version reads no configuration, so it runs where none parses.
		c.cmd, _ = lookupCommand(verb)
		return c.cmd.run(c, rest)
	case "mesh":
		return errors.New(meshRemoved)
	}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	if verb == "serve" {
		// serve keeps its own flags and signal handling; resolved globals go first so its flags still win.
		return serve(append([]string{"-state", c.state, "-mount", c.mount}, rest...))
	}
	cmd, ok := lookupCommand(verb)
	if !ok {
		return fmt.Errorf("unknown command %q; run 'ovnfs help'", verb)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c.ctx, c.autostart, c.cmd = ctx, cmd.autostart, cmd
	defer c.close()
	return cmd.run(c, rest)
}

// resolveDirs fills the state and mount folders --state and --mount left unset
// from OVNFS_STATE and OVNFS_MOUNT, so scripts can point every verb at another
// daemon, then from state and mount in ~/.o2/ovnfs.yml, then the defaults.
func (c *cli) resolveDirs() error {
	c.state, c.mount = cmp.Or(c.state, os.Getenv("OVNFS_STATE")), cmp.Or(c.mount, os.Getenv("OVNFS_MOUNT"))
	var conf manifest.Daemon
	if home, err := os.UserHomeDir(); err == nil && (c.state == "" || c.mount == "") {
		if conf, err = manifest.UserDaemon(home); err != nil {
			return err
		}
	}
	c.state, c.mount = cmp.Or(c.state, conf.State, defaultState), cmp.Or(c.mount, conf.Mount, defaultMount)
	return nil
}

// build names this binary as `ovnfs --json version` prints it: the module
// version Go stamped into it (the release tag under go install …@vX.Y.Z, a VCS
// pseudo-version when built in a clone, else "(devel)"), the toolchain and the platform.
type build struct {
	Version string `json:"version"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// runVersion prints this binary's build on one line, or as JSON under --json.
func runVersion(c *cli, args []string) error {
	if _, err := c.parse(c.flags("version"), args, 0, 0); err != nil {
		return err
	}
	b := build{Version: "(devel)", Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		b.Version = bi.Main.Version
	}
	if c.json {
		return json.NewEncoder(os.Stdout).Encode(b)
	}
	fmt.Printf("ovnfs %s %s %s/%s\n", b.Version, b.Go, b.OS, b.Arch)
	return nil
}

// printUsage lists every verb, daemon operations first in api.Ops order.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: ovnfs [--state DIR] [--mount DIR] [--json] VERB [ARGS]")
	fmt.Fprintln(w)
	cmds := slices.Clone(commands())
	rank := func(name string) int {
		if i := slices.Index(api.Ops, name); i >= 0 {
			return i
		}
		return len(api.Ops)
	}
	slices.SortStableFunc(cmds, func(a, b command) int { return rank(a.name) - rank(b.name) })
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, cmd := range cmds {
		fmt.Fprintf(tw, "  %s %s\t%s\n", cmd.name, cmd.args, cmd.desc)
	}
	fmt.Fprintf(tw, "  serve [-proto nfs|smb|fuse|winfsp] [-listen ADDR]\trun the daemon in the foreground\n")
	fmt.Fprintf(tw, "  help\tshow this help\n")
	tw.Flush()
	fmt.Fprintln(w)
	fmt.Fprintln(w, "options (before the verb or among its arguments):")
	fmt.Fprintln(w, "  --state DIR   daemon state, env OVNFS_STATE, state in ~/.o2/ovnfs.yml (default "+defaultState+")")
	fmt.Fprintln(w, "  --mount DIR   checkout mount (a free drive letter on Windows), env OVNFS_MOUNT, mount in ~/.o2/ovnfs.yml (default "+defaultMount+")")
	fmt.Fprintln(w, "  --json        print the raw reply JSON")
}
