// Command ovnfs opens copy-on-write checkouts of any folder and serves them to agents.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"

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
		fmt.Fprintln(stderr, colorError(err))
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

// run resolves the global options, then parses and runs one verb.
func run(args []string) error {
	c := &cli{autostart: true}
	args, err := splitGlobals(c, args)
	if err != nil {
		return err
	}
	if err := c.setColor(); err != nil {
		return err
	}
	if len(args) == 0 {
		return printHelp(c, nil)
	}
	if args[0] == "help" {
		return printHelp(c, args[1:])
	}
	// version and mesh read no configuration, so they run where none parses;
	// config reads it itself, so a broken file can still be fixed with it.
	if verb := args[0]; verb != "version" && verb != "mesh" && verb != "config" {
		if err := c.resolveDirs(); err != nil {
			return err
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c.ctx = ctx
	defer c.close()
	return runCLI(c, args)
}

// meshCmd answers the removed `ovnfs mesh` verb; it stays out of help.
type meshCmd struct{}

func (cmd *meshCmd) Run(c *cli) error {
	return errors.New(meshRemoved)
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

// versionCmd prints this binary's build on one line, or as JSON under --json.
type versionCmd struct{}

func (cmd *versionCmd) Run(c *cli) error {
	c.autostart = false
	b := build{Version: "(devel)", Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		b.Version = bi.Main.Version
	}
	if c.json {
		return json.NewEncoder(stdout).Encode(b)
	}
	fmt.Fprintf(stdout, "ovnfs %s %s %s/%s\n", b.Version, b.Go, b.OS, b.Arch)
	return nil
}
