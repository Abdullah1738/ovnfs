package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
)

// cli carries the resolved options and the lazily dialed daemon connection of one invocation.
type cli struct {
	ctx       context.Context
	state     string
	mount     string
	json      bool
	color     string // --color: auto, always or never
	autostart bool   // start the daemon when its socket does not accept connections
	cmd       command
	conn      *admin.Client
}

// command is one CLI verb: its usage, help line and implementation.
type command struct {
	name      string
	args      string
	desc      string
	group     string // help section, one of helpGroups
	autostart bool
	run       func(c *cli, args []string) error
}

// Help sections, in the order `ovnfs help` prints them.
const (
	groupCheckouts = "checkouts"
	groupInspect   = "inspect"
	groupCleanup   = "clean up"
	groupRemote    = "other machines"
	groupDaemon    = "daemon"
	groupBinary    = "ovnfs itself"
)

// helpGroups orders the sections of `ovnfs help`.
var helpGroups = []string{groupCheckouts, groupInspect, groupCleanup, groupRemote, groupDaemon, groupBinary}

// helpExtras are the verbs serve and help, which run before a resolved
// configuration and so stay out of commands(); they still take a help row.
var helpExtras = []command{
	{name: "serve", args: "[-proto nfs|smb|fuse|winfsp] [-listen ADDR]", desc: "run the daemon in the foreground", group: groupDaemon},
	{name: "help", desc: "show this help", group: groupBinary},
}

// commands returns the verb table, ordered by help section; a function so help
// can refer to it without an init cycle.
func commands() []command {
	return []command{
		{"ls", "[CHECKOUT|DIR] [--idle DUR] [--no-diff]", "list checkouts (of DIR: . or a path with /), or one checkout's snapshots", groupCheckouts, true, runLs},
		{"new", "[DIR] [--name N] [--rev REV | --from SNAP] [--take SRC:PATH]... [--rule LINE]...", "open a checkout on a folder (default .), git revision, or snapshot", groupCheckouts, true, runNew},
		{"run", "[DIR] [--rev REV] [--take SRC:PATH]... [--keep] [--on HOST] -- CMD...", "run a command in a disposable checkout (also --name, --rule, --remote-bin)", groupCheckouts, true, runRun},
		{"fork", "CHECKOUT [--name N] [-m LABEL]", "snapshot a checkout and open a second checkout on it", groupCheckouts, true, runFork},
		{"snap", "CHECKOUT [-m LABEL] [--wait]", "freeze a checkout's changes into a snapshot", groupCheckouts, true, runSnap},
		{"diff", "CHECKOUT [SNAP] [-p|--patch] [--binary]", "list source changes since a snapshot (default: the base)", groupCheckouts, true, runDiff},
		{"apply", "CHECKOUT [--dry-run] [--branch B]", "merge a checkout's source changes back into its folder", groupCheckouts, true, runApply},
		{"grep", "PATTERN [DIR|CHECKOUT] [--glob GLOB] [-F] [-i] [-C N] [--max-count N]", "search contents in the daemon, bypassing the checkout mount", groupInspect, true, runGrep},
		{"glob", "PATTERN [DIR|CHECKOUT] [--type file|dir|symlink] [--limit N] [--mtime]", "list matching paths in the daemon, bypassing the checkout mount", groupInspect, true, runGlob},
		{"explain", "[DIR|CHECKOUT] PATH [--rule LINE]...", "show which class a path gets and which rule decided", groupInspect, true, runExplain},
		{"plan", "[DIR|CHECKOUT] [--on cloud|peer] [--rule LINE]...", "show what a launch elsewhere would transfer, rebuild and leave", groupInspect, true, runPlan},
		{"rm", "CHECKOUT [--sticky-snapshots]", "remove a checkout and its unused snapshots; --sticky-snapshots retains them", groupCleanup, true, runRm},
		{"prune", "[DIR] [--idle DUR] [--no-diff] [--dry-run] [--sticky-snapshots]", "remove matching checkouts and unused snapshots; --sticky-snapshots retains them", groupCleanup, true, runPrune},
		{"drop", "SNAP", "release a snapshot", groupCleanup, true, runDrop},
		{"attach", "[DIR|CHECKOUT] --to HOST [--name N] [--remote-bin PATH]", "make a checkout appear on another machine over ssh; content moves on first read", groupRemote, true, runAttach},
		{"send", "[DIR|CHECKOUT] HOST[:PATH] [--name N] [--remote-bin PATH]", "attach over ssh, first installing ovnfs there if missing; PATH there links to the checkout", groupRemote, true, runSend},
		{"pull", "CHECKOUT [--from HOST] [--dry-run]", "merge attached remote source changes into the local checkout", groupRemote, true, runPull},
		{"push", "[CHECKOUT] [--to HOST]", "have attached machines fetch all content they still read from this one, so they work without it", groupRemote, true, runPush},
		{"share", "CHECKOUT [--rw]", "serve a checkout over remote-fs for a VM (vmon --remote-fs TAG:SOCK[:rw]) or a peer", groupRemote, true, runShare},
		{"link", "", "pipe an attach session from ssh into this machine's daemon (run by attach)", groupRemote, true, runLink},
		{"status", "", "show the running daemon", groupDaemon, false, runStatus},
		{"start", "[--json]", "make sure the daemon runs (starting it in the background); --json prints its status and socket", groupDaemon, false, runStart},
		{"stop", "", "stop the daemon", groupDaemon, false, runStop},
		{"restart", "[--json]", "stop the daemon and start a new one from this binary; --json prints the new one's status", groupDaemon, false, runRestart},
		{"subscribe", "", "stream daemon events as JSON lines until interrupted", groupDaemon, true, runSubscribe},
		{"ctl", "OP ['JSON']", "send one raw admin request and print the raw reply", groupDaemon, true, runCtl},
		{"profile", "[KIND] [--seconds D] [-o FILE]", "capture a runtime profile from the daemon (cpu, heap, allocs, goroutine, block, mutex, threadcreate)", groupDaemon, true, runProfile},
		{"version", "[--json]", "print the version and toolchain this binary was built from", groupBinary, false, runVersion},
		{"update", "", "replace this binary with the latest ovnfs, by go install if it built it, else from the latest release", groupBinary, false, runUpdate},
	}
}

// lookupCommand finds a verb by name.
func lookupCommand(name string) (command, bool) {
	for _, cmd := range commands() {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

// stringList is a repeatable string flag such as --rule.
type stringList []string

// String renders the collected values for flag defaults.
func (s *stringList) String() string { return strings.Join(*s, ", ") }

// Set appends one occurrence.
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// flags returns a quiet flag set that already accepts the global options.
func (c *cli) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.state, "state", c.state, "daemon state directory")
	fs.StringVar(&c.mount, "mount", c.mount, "checkout mount directory")
	fs.BoolVar(&c.json, "json", c.json, "print the raw reply JSON")
	fs.StringVar(&c.color, "color", "auto", "colour the output: auto, always or never")
	return fs
}

// parseInterspersed parses flags anywhere among positionals; everything after "--" is positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// parse reads the verb's flags and checks its positional count against the usage.
func (c *cli) parse(fs *flag.FlagSet, args []string, minPos, maxPos int) ([]string, error) {
	pos, err := parseInterspersed(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(stdout, "%s ovnfs %s%s\n  %s\n", paint(stdout, styleBold, "usage:"),
			paint(stdout, styleAccent, c.cmd.name), paintIf(stdout, c.cmd.args != "", styleDim, " "+c.cmd.args), c.cmd.desc)
		return nil, exitCode(0)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", c.cmd.name, err, c.usage())
	}
	if err := c.setColor(); err != nil {
		return nil, err
	}
	if len(pos) < minPos || len(pos) > maxPos {
		return nil, errors.New(c.usage())
	}
	return pos, nil
}

// usage is the one-line synopsis of the running verb.
func (c *cli) usage() string {
	return strings.TrimSpace("usage: ovnfs " + paint(stdout, styleAccent, c.cmd.name) + paintIf(stdout, c.cmd.args != "", styleDim, " "+c.cmd.args))
}

// helpRows is the `ovnfs help` table: every command plus the extras.
func helpRows() []command {
	return append(commands(), helpExtras...)
}

// call sends one operation and returns the raw reply, starting the daemon if allowed.
func (c *cli) call(op string, args any) (json.RawMessage, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	return client.Call(c.ctx, op, args)
}

// close releases the daemon connection, if one was made.
func (c *cli) close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

// call performs op, prints the raw reply under --json, and decodes it for human output.
func call[T any](c *cli, op string, args any) (T, error) {
	raw, err := c.call(op, args)
	if err == nil && c.json {
		fmt.Println(string(raw))
	}
	return decodeReply[T](op, raw, err)
}

// decodeReply decodes the raw reply to op unless the call failed.
func decodeReply[T any](op string, raw json.RawMessage, err error) (T, error) {
	var reply T
	if err != nil {
		return reply, err
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return reply, fmt.Errorf("%s: decoding reply: %w", op, err)
	}
	return reply, nil
}

// runLs lists checkouts, those of a folder or picked by flags, or the snapshots
// of one checkout. A path spelling (".", "..", a slash) makes the argument a
// folder, so a checkout named after a folder beside it stays reachable by name.
func runLs(c *cli, args []string) error {
	fs := c.flags("ls")
	var a api.LsArgs
	selectFlags(fs, &a.Select)
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	name := ""
	if len(pos) == 1 && !pathArg(pos[0]) {
		name = pos[0]
		if a.Select != (api.Select{}) {
			return fmt.Errorf("ls: --idle and --no-diff pick checkouts, not snapshots of %s", name)
		}
	} else if len(pos) == 1 {
		if a.Dir, err = filepath.Abs(pos[0]); err != nil {
			return fmt.Errorf("ls: resolving %s: %w", pos[0], err)
		}
	}
	r, err := call[api.LsReply](c, "ls", a)
	if err != nil || c.json {
		return err
	}
	if name == "" {
		printCheckouts(stdout, r)
		return nil
	}
	rows := [][]string{{paint(stdout, styleBold, "ID"), paint(stdout, styleBold, "LABEL"), paint(stdout, styleBold, "CREATED"), paint(stdout, styleBold, "TREE")}}
	header := false
	for _, s := range r.Snapshots {
		if s.Checkout != name {
			continue
		}
		header = true
		rows = append(rows, []string{paint(stdout, styleAccent, s.ID), s.Label, paint(stdout, styleDim, stamp(s.Created)), treeCell(stdout, s.Tree)})
	}
	if header {
		table(stdout, rows)
	}
	if !header && !slices.ContainsFunc(r.Checkouts, func(h api.Checkout) bool { return h.Name == name }) {
		if isDirArg(name) {
			return fmt.Errorf("ls: no checkout %s; for the folder, ls ./%s", name, name)
		}
		return fmt.Errorf("ls: no checkout %s", name)
	}
	return nil
}

// printCheckouts lists live checkouts and retained snapshots without a live owner.
func printCheckouts(w io.Writer, r api.LsReply) {
	snaps := make(map[string]int)
	live := make(map[string]bool)
	for _, s := range r.Snapshots {
		snaps[s.Checkout]++
	}
	if len(r.Checkouts) > 0 {
		rows := [][]string{{paint(w, styleBold, "CHECKOUT"), paint(w, styleBold, "SOURCE"), paint(w, styleBold, "SNAPS"), paint(w, styleBold, "CHANGED"), paint(w, styleBold, "TOUCHED"), paint(w, styleBold, "PATH")}}
		for _, h := range r.Checkouts {
			live[h.Name] = true
			rows = append(rows, []string{
				paint(w, styleAccent.Bold(true), h.Name),
				paint(w, styleDim, h.Source),
				fmt.Sprint(snaps[h.Name]),
				paintIf(w, h.Dirty, styleYellow, paintIf(w, !h.Dirty, styleGreen, yesNo(h.Dirty))),
				paint(w, styleDim, stamp(h.Touched)),
				h.Path,
			})
		}
		table(w, rows)
	}
	var orphans [][]string
	for _, s := range r.Snapshots {
		if live[s.Checkout] {
			continue
		}
		if len(orphans) == 0 {
			orphans = append(orphans, []string{paint(w, styleBold, "ORPHAN SNAPSHOT"), paint(w, styleBold, "CHECKOUT"), paint(w, styleBold, "LABEL"), paint(w, styleBold, "CREATED"), paint(w, styleBold, "TREE")})
		}
		orphans = append(orphans, []string{paint(w, styleAccent, s.ID), paint(w, styleAccent, s.Checkout), s.Label, paint(w, styleDim, stamp(s.Created)), treeCell(w, s.Tree)})
	}
	if len(orphans) > 0 {
		if len(r.Checkouts) > 0 {
			fmt.Fprintln(w)
		}
		table(w, orphans)
	}
}

// treeCell renders a snapshot's content id: abbreviated and dim, or a placeholder
// while the daemon hashes it.
func treeCell(w io.Writer, tree string) string {
	if tree == "" {
		return paint(w, styleDim, "hashing…")
	}
	return paint(w, styleDim, abbrev(tree, 12))
}

// selectFlags registers the flags ls and prune pick checkouts by (see api.Select).
func selectFlags(fs *flag.FlagSet, sel *api.Select) {
	fs.Var((*seconds)(&sel.Idle), "idle", "only checkouts untouched for this long: 90m, 12h, 7d, 2w")
	fs.BoolVar(&sel.NoDiff, "no-diff", false, "only checkouts without source changes since their base (an empty diff)")
}

// seconds is a duration flag held in seconds, as api fields are; besides Go
// durations (90m, 12h) it takes whole days and weeks (7d, 2w).
type seconds float64

// String renders the duration for flag defaults.
func (s *seconds) String() string { return time.Duration(float64(*s) * float64(time.Second)).String() }

// Set parses one duration.
func (s *seconds) Set(v string) error {
	d, err := time.ParseDuration(v)
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(v, suffix); ok {
			var whole uint64
			whole, err = strconv.ParseUint(n, 10, 12) // up to 4095 weeks, within a Duration
			d = time.Duration(whole) * unit
		}
	}
	if err != nil || d < 0 {
		return fmt.Errorf("invalid duration %q", v)
	}
	*s = seconds(d.Seconds())
	return nil
}

// runPrune removes the checkouts a folder and flags pick and prints their names.
func runPrune(c *cli, args []string) error {
	fs := c.flags("prune")
	var a api.PruneArgs
	selectFlags(fs, &a.Select)
	fs.BoolVar(&a.StickySnapshots, "sticky-snapshots", false, "retain snapshots of removed checkouts")
	fs.BoolVar(&a.DryRun, "dry-run", false, "print what would be removed; remove nothing")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		if a.Dir, err = filepath.Abs(pos[0]); err != nil {
			return fmt.Errorf("prune: resolving %s: %w", pos[0], err)
		}
	}
	if a.Select == (api.Select{}) {
		return fmt.Errorf("prune: pick checkouts by DIR, --idle or --no-diff\n%s", c.usage())
	}
	r, err := call[api.PruneReply](c, "prune", a)
	if err != nil || c.json {
		return err
	}
	for _, h := range r.Removed {
		fmt.Fprintln(stdout, h.Name)
	}
	switch n := plural(len(r.Removed), "checkout", "checkouts"); {
	case len(r.Removed) == 0:
		fmt.Fprintln(stderr, paint(stderr, styleYellow, "no checkout matches"))
	case a.DryRun:
		fmt.Fprintf(stderr, "%s %s\n", paint(stderr, styleYellow, "would remove"), n)
	case a.StickySnapshots:
		fmt.Fprintf(stderr, "%s %s; snapshots retained\n", paint(stderr, styleGreen, "removed"), n)
	default:
		fmt.Fprintf(stderr, "%s %s and their snapshots\n", paint(stderr, styleGreen, "removed"), n)
	}
	return nil
}

// runNew opens a checkout on a folder, a snapshot or a capture and prints its path.
func runNew(c *cli, args []string) error {
	fs := c.flags("new")
	var a api.NewArgs
	fs.StringVar(&a.Name, "name", "", "checkout name")
	fs.StringVar(&a.Snapshot, "from", "", "open on this snapshot instead of a folder")
	fs.StringVar(&a.Base, "base", "", "open on this capture (a checkout's base) as it was taken, instead of a folder")
	fs.StringVar(&a.Rev, "rev", "", "start at this git commit or local branch; keep ignored files")
	fs.Var((*stringList)(&a.Take), "take", "replace source path from working tree (.), index (empty), or revision (repeatable)")
	fs.Var((*stringList)(&a.Rules), "rule", "extra rule line (repeatable)")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if a.Snapshot != "" && a.Base != "" {
		return fmt.Errorf("new: give either --from SNAP or --base CAPTURE, not both")
	}
	if a.Snapshot != "" || a.Base != "" {
		if a.Rev != "" {
			return fmt.Errorf("new: --rev needs a folder")
		}
		if len(a.Take) != 0 {
			return fmt.Errorf("new: --take needs a folder")
		}
		if len(pos) != 0 {
			return fmt.Errorf("new: give either DIR, --from SNAP or --base CAPTURE")
		}
	} else {
		dir := "."
		if len(pos) == 1 {
			dir = pos[0]
		}
		if a.Dir, err = filepath.Abs(dir); err != nil {
			return fmt.Errorf("new: resolving %s: %w", dir, err)
		}
	}
	r, err := call[api.NewReply](c, "new", a)
	if err != nil || c.json {
		return err
	}
	fmt.Fprintln(stdout, r.Checkout.Path)
	from := r.Checkout.Source
	if a.Snapshot != "" {
		from = strings.TrimPrefix(from+"@", "@") + a.Snapshot
	}
	fmt.Fprintf(stderr, "%s  %s  %s\n", paint(stderr, styleAccent.Bold(true), r.Checkout.Name), paint(stderr, styleDim, "←"), paint(stderr, styleDim, from))
	if cp := r.Capture; cp != nil {
		fmt.Fprintf(stderr, "  %s %s files (%s) · %s %s (%s) in %.1fs\n", paint(stderr, styleDim, "source"),
			commas(int64(cp.Files)), humanBytes(cp.Bytes), paint(stderr, styleDim, "hashed"), commas(int64(cp.Hashed)), humanBytes(cp.HashedBytes), cp.Seconds)
		git := "none"
		if len(cp.Repos) > 0 {
			repos := make([]string, len(cp.Repos))
			for i, r := range cp.Repos {
				repos[i] = orDefault(r, ".")
			}
			git = strings.Join(repos, ", ")
		}
		fmt.Fprintf(stderr, "  %s · %s · %s · %s\n",
			paint(stderr, styleDim, "cache "+commas(int64(cp.Cache))),
			paint(stderr, styleDim, "local "+commas(int64(cp.Local))),
			paint(stderr, styleDim, "hidden "+commas(int64(cp.Hidden))),
			paint(stderr, styleDim, "git: "+git))
	}
	for _, w := range r.Warnings {
		fmt.Fprintln(stderr, paint(stderr, styleYellow, "warning:"), w)
	}
	return nil
}

// runFork snapshots a checkout, opens a child on it and prints the child's path.
func runFork(c *cli, args []string) error {
	fs := c.flags("fork")
	var a api.ForkArgs
	fs.StringVar(&a.Name, "name", "", "child checkout name")
	fs.StringVar(&a.Label, "m", "", "snapshot label")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	a.Checkout = pos[0]
	r, err := call[api.ForkReply](c, "fork", a)
	if err != nil || c.json {
		return err
	}
	fmt.Fprintln(stdout, r.Checkout.Path)
	fmt.Fprintf(stderr, "%s  %s  %s@%s\n", paint(stderr, styleAccent.Bold(true), r.Checkout.Name), paint(stderr, styleDim, "←"),
		paint(stderr, styleAccent, a.Checkout), paint(stderr, styleDim, r.Snapshot.ID))
	return nil
}

// runSnap freezes a checkout into a snapshot and prints its id.
func runSnap(c *cli, args []string) error {
	fs := c.flags("snap")
	var a api.SnapArgs
	fs.StringVar(&a.Label, "m", "", "snapshot label")
	fs.BoolVar(&a.Wait, "wait", false, "wait until the tree id is known")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	a.Checkout = pos[0]
	r, err := call[api.SnapReply](c, "snap", a)
	if err != nil || c.json {
		return err
	}
	fmt.Fprintln(stdout, r.Snapshot.ID)
	line := fmt.Sprintf("%s %s %s %s", paint(stderr, styleDim, "snapshot"), paint(stderr, styleAccent, r.Snapshot.ID),
		paint(stderr, styleDim, "of"), paint(stderr, styleAccent, a.Checkout))
	if r.Snapshot.Tree != "" {
		line += " " + paint(stderr, styleDim, "tree "+abbrev(r.Snapshot.Tree, 12))
	}
	fmt.Fprintln(stderr, line)
	return nil
}

// runDiff prints a checkout's changed source paths and, on request, the patch.
func runDiff(c *cli, args []string) error {
	fs := c.flags("diff")
	var a api.DiffArgs
	fs.BoolVar(&a.Patch, "p", false, "print a unified diff")
	fs.BoolVar(&a.Patch, "patch", false, "print a unified diff")
	fs.BoolVar(&a.Binary, "binary", false, "print the patch as git diff --binary does, for git apply")
	pos, err := c.parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	a.Checkout = pos[0]
	if len(pos) == 2 {
		a.From = pos[1]
	}
	r, err := call[api.DiffReply](c, "diff", a)
	if err != nil || c.json {
		return err
	}
	for _, ch := range r.Changes {
		fmt.Fprintf(stdout, "%s\t%s\n", mark(stdout, ch.Status), ch.Path)
	}
	if r.Patch != "" {
		colorPatch(stdout, r.Patch)
		if !strings.HasSuffix(r.Patch, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	return nil
}

// runApply merges a checkout back into its source and fails when conflicts were left.
func runApply(c *cli, args []string) error {
	fs := c.flags("apply")
	var a api.ApplyArgs
	fs.BoolVar(&a.DryRun, "dry-run", false, "report without writing")
	fs.StringVar(&a.Branch, "branch", "", "fetch commits as this branch, or fast-forward it and its index if checked out")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	a.Checkout = pos[0]
	r, err := call[api.ApplyReply](c, "apply", a)
	if err != nil {
		return err
	}
	return printMerge(c, r, a.DryRun, "ovnfs apply "+a.Checkout)
}

// runPull merges one attached remote checkout's source changes into the local checkout.
func runPull(c *cli, args []string) error {
	fs := c.flags("pull")
	var a api.PullArgs
	fs.StringVar(&a.From, "from", "", "host passed to attach; optional with one attachment")
	fs.BoolVar(&a.DryRun, "dry-run", false, "report without writing")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	a.Checkout = pos[0]
	r, err := call[api.PullReply](c, "pull", a)
	if err != nil {
		return err
	}
	again := "ovnfs pull " + a.Checkout
	if a.From != "" {
		again += " --from " + a.From
	}
	return printMerge(c, r.ApplyReply, a.DryRun, again)
}

// printMerge prints apply and pull outcomes and fails when conflicts remain,
// asking the user to resolve conflict markers and then run again.
func printMerge(c *cli, r api.ApplyReply, dryRun bool, again string) error {
	if !c.json {
		if dryRun {
			fmt.Fprintln(stderr, paint(stderr, styleYellow, "dry run: nothing written"))
		}
		for _, ch := range r.Applied {
			if r.Branch != "" {
				fmt.Fprintf(stdout, "%s %s\t%s\n", paint(stdout, styleDim, "index"), mark(stdout, ch.Status), ch.Path)
			} else {
				fmt.Fprintf(stdout, "%s\t%s\n", mark(stdout, ch.Status), ch.Path)
			}
		}
		for _, p := range r.Merged {
			fmt.Fprintf(stdout, "%s %s\n", paint(stdout, styleBlue, "merged"), p)
		}
		for _, cf := range r.Conflicts {
			verb := "conflict"
			if cf.Markers {
				verb = "marked"
			}
			fmt.Fprintf(stdout, "%s %s: %s\n", paint(stdout, styleRed, verb), cf.Path, paint(stdout, styleDim, cf.Reason))
		}
		if r.Branch != "" {
			fmt.Fprintf(stdout, "%s %s %s %s\n", paint(stdout, styleDim, "branch"), paint(stdout, styleAccent, r.Branch), paint(stdout, styleDim, "at"), paint(stdout, styleDim, abbrev(r.Commit, 12)))
		}
	}
	if len(r.Conflicts) == 0 {
		return nil
	}
	marked := 0
	for _, cf := range r.Conflicts {
		if cf.Markers {
			marked++
		}
	}
	if untouched := len(r.Conflicts) - marked; untouched > 0 {
		fmt.Fprintf(stderr, "%s left untouched\n", paint(stderr, styleYellow, plural(untouched, "conflict", "conflicts")))
	}
	switch {
	case marked > 0 && dryRun:
		fmt.Fprintf(stderr, "%s would have conflict markers\n", paint(stderr, styleYellow, plural(marked, "file", "files")))
	case marked > 0:
		fmt.Fprintf(stderr, "%s conflict markers: resolve them, then run `%s` again\n", paint(stderr, styleYellow, plural(marked, "file has", "files have")), paint(stderr, styleAccent, again))
	}
	return exitCode(1)
}

// runRm removes a checkout.
func runRm(c *cli, args []string) error {
	fs := c.flags("rm")
	var a api.RmArgs
	fs.BoolVar(&a.StickySnapshots, "sticky-snapshots", false, "retain snapshots of the removed checkout")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	a.Checkout = pos[0]
	_, err = call[json.RawMessage](c, "rm", a)
	return err
}

// runAttach offers a folder or checkout to another machine and prints where it appeared.
func runAttach(c *cli, args []string) error {
	fs := c.flags("attach")
	to := fs.String("to", "", "ssh destination of the remote machine")
	name := fs.String("name", "", "checkout name on the remote machine")
	bin := fs.String("remote-bin", "", "path of ovnfs on the remote machine (default: ovnfs on its PATH)")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if *to == "" {
		return fmt.Errorf("attach needs --to HOST")
	}
	target := "."
	if len(pos) == 1 {
		target = pos[0]
	}
	dir, checkoutName, err := resolveTarget(target)
	if err != nil {
		return err
	}
	reply, err := call[api.AttachReply](c, "attach", api.AttachArgs{Dir: dir, Checkout: checkoutName, To: *to, Name: *name, RemoteBin: *bin})
	if err != nil || c.json {
		return err
	}
	printAttached(*to, reply.RemotePath, reply)
	return nil
}

// printAttached reports an attach: host:path on stdout for scripts, the
// checkout and link on stderr.
func printAttached(host, path string, r api.AttachReply) {
	fmt.Fprintf(stdout, "%s:%s\n", host, path)
	fmt.Fprintf(stderr, "%s@%s  %s  %s:%s %s\n", paint(stderr, styleAccent.Bold(true), r.Checkout), paint(stderr, styleDim, r.Snapshot.ID),
		paint(stderr, styleDim, "→"), paint(stderr, styleAccent, r.Host), paint(stderr, styleAccent, r.RemoteCheckout),
		paint(stderr, styleDim, "(content moves on first read; the link reconnects while this daemon runs)"))
}

// runLink pipes stdin and stdout to the daemon's link socket; attach runs it over ssh.
// Nothing else may be written to stdout: it carries the link protocol.
func runLink(c *cli, args []string) error {
	if _, err := c.parse(c.flags("link"), args, 0, 0); err != nil {
		return err
	}
	if _, err := c.client(); err != nil {
		return err
	}
	conn, err := net.Dial("unix", filepath.Join(c.state, linkSocket))
	if err != nil {
		return fmt.Errorf("connecting to the link socket: %w", err)
	}
	defer conn.Close()
	go func() {
		io.Copy(conn, os.Stdin)
		if u, ok := conn.(*net.UnixConn); ok {
			u.CloseWrite()
		}
	}()
	_, err = io.Copy(os.Stdout, conn)
	return err
}

// runShare prints the remote-fs socket of a checkout, starting its share if needed.
func runShare(c *cli, args []string) error {
	fs := c.flags("share")
	rw := fs.Bool("rw", false, "let the client write")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	reply, err := call[api.ShareReply](c, "share", api.ShareArgs{Checkout: pos[0], Writable: *rw})
	if err != nil || c.json {
		return err
	}
	fmt.Fprintln(stdout, reply.Socket)
	mode := ""
	if reply.Writable {
		mode = ":rw"
	}
	fmt.Fprintf(stderr, "%s home:%s\n", paint(stderr, styleAccent, "vmon --remote-fs"), paint(stderr, styleDim, reply.Socket+mode))
	return nil
}

// runDrop releases a snapshot.
func runDrop(c *cli, args []string) error {
	pos, err := c.parse(c.flags("drop"), args, 1, 1)
	if err != nil {
		return err
	}
	_, err = call[json.RawMessage](c, "drop", api.DropArgs{Snapshot: pos[0]})
	return err
}

// runExplain prints the class of one path and the rule that decided it.
func runExplain(c *cli, args []string) error {
	fs := c.flags("explain")
	var a api.ExplainArgs
	fs.Var((*stringList)(&a.Rules), "rule", "extra rule line (repeatable)")
	pos, err := c.parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	target, path := "", pos[0]
	if len(pos) == 2 {
		target, path = pos[0], pos[1]
	}
	if a.Dir, a.Checkout, err = resolveTarget(target); err != nil {
		return fmt.Errorf("explain: %w", err)
	}
	if a.Path, err = relativeTo(a.Dir, path); err != nil {
		return fmt.Errorf("explain: %w", err)
	}
	r, err := call[api.ExplainReply](c, "explain", a)
	if err != nil || c.json {
		return err
	}
	line := paint(stdout, classStyle(r.Class), r.Class) + strings.Repeat(" ", max(1, 9-len(r.Class))) +
		paint(stdout, styleDim, r.Rule.Origin) + "  " + paint(stdout, styleAccent, r.Rule.Class) + " " + r.Rule.Text
	if r.At != "" && r.At != r.Path {
		line += paint(stdout, styleDim, " (decided at "+r.At+"/)")
	}
	if !r.Exists {
		line += paint(stdout, styleDim, " (no such path; classified by name)")
	}
	fmt.Fprintln(stdout, line)
	return nil
}

// runPlan prints how a launch elsewhere would recreate a folder or checkout.
func runPlan(c *cli, args []string) error {
	fs := c.flags("plan")
	a := api.PlanArgs{On: "cloud"}
	fs.StringVar(&a.On, "on", a.On, "cloud or peer")
	fs.Var((*stringList)(&a.Rules), "rule", "extra rule line (repeatable)")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	target := ""
	if len(pos) == 1 {
		target = pos[0]
	}
	if a.Dir, a.Checkout, err = resolveTarget(target); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	r, err := call[api.PlanReply](c, "plan", a)
	if err != nil || c.json {
		return err
	}
	printPlan(os.Stdout, r)
	return nil
}

// isDirArg reports whether a DIR|CHECKOUT argument names a folder rather than a checkout.
func isDirArg(s string) bool {
	if pathArg(s) {
		return true
	}
	info, err := os.Stat(s)
	return err == nil && info.IsDir()
}

// pathArg reports whether an argument is spelled as a path: ".", "..", or with a separator.
func pathArg(s string) bool {
	return s == "." || s == ".." || strings.ContainsRune(s, '/') || strings.ContainsRune(s, filepath.Separator)
}

// resolveTarget turns an optional DIR|CHECKOUT argument into exactly one of an absolute dir or a checkout name.
func resolveTarget(arg string) (dir, checkout string, err error) {
	if arg != "" && !isDirArg(arg) {
		return "", arg, nil
	}
	if dir, err = filepath.Abs(orDefault(arg, ".")); err != nil {
		return "", "", fmt.Errorf("resolving %s: %w", arg, err)
	}
	return dir, "", nil
}

// relativeTo converts a user path into the slash-separated form relative to dir ("" for a checkout's root).
func relativeTo(dir, path string) (string, error) {
	if dir == "" {
		if filepath.IsAbs(path) {
			return "", fmt.Errorf("path %s must be relative to the checkout", path)
		}
		dir = string(filepath.Separator)
		path = filepath.Join(dir, path)
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", fmt.Errorf("path %s: %w", path, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path %s is outside %s", path, dir)
	}
	if rel == "." {
		return "", nil
	}
	return rel, nil
}

// printPlan renders a plan as one header line and one labelled block per class.
func printPlan(w io.Writer, r api.PlanReply) {
	fmt.Fprintf(w, "%s %s %s     %s\n", paint(w, styleAccent, r.Root), paint(w, styleDim, "→"), r.On, paint(w, styleDim, gitSummary(r.Repos)))
	const indent = "         " // width of the class label column
	label := func(s string) string { return paint(w, styleDim, s) + strings.Repeat(" ", max(0, 9-len(s))) }
	counts := []string{
		plural(r.Source.Files, "file", "files"),
		plural(r.Local.Files, "path", "paths"),
		plural(r.Hidden.Files, "path", "paths"),
	}
	width := 0
	for _, s := range counts {
		width = max(width, len(s))
	}
	column := func(count string) string { return fmt.Sprintf("%-*s   ", width, count) }

	var parts []string
	if r.Origin.Files > 0 {
		at := ""
		if len(r.Repos) == 1 && r.Repos[0].Head != "" {
			at = " @" + abbrev(r.Repos[0].Head, 7)
		}
		parts = append(parts, commas(int64(r.Origin.Files))+" from origin"+at)
	}
	if r.Here.Files > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%s from this machine · %s%s", commas(int64(r.Here.Files)), humanBytes(r.Here.Bytes), pathSample(r.Here, 3)))
	}
	fmt.Fprintf(w, "%s%s%s\n", label("source"), column(counts[0]), parts[0])
	for _, p := range parts[1:] {
		fmt.Fprintf(w, "%s%s%s\n", indent, strings.Repeat(" ", width+3), p)
	}

	if len(r.Cache) == 0 {
		fmt.Fprintf(w, "%s%s\n", label("cache"), paint(w, styleDim, "none"))
	}
	rootWidth := 0
	for _, cache := range r.Cache {
		rootWidth = max(rootWidth, len(cache.Root))
	}
	for i, cache := range r.Cache {
		prefix := indent
		if i == 0 {
			prefix = label("cache")
		}
		action := paint(w, styleDim, "starts empty")
		if cache.Rebuild != "" {
			action = paint(w, styleDim, "rebuild: ") + cache.Rebuild
		}
		if cache.KeyHash != "" {
			key := cache.KeyHash
			if len(key) > 11 {
				key = key[:11] + "…"
			}
			action += paint(w, styleDim, "  key "+key)
		}
		fmt.Fprintf(w, "%s%-*s  %s\n", prefix, rootWidth, cache.Root, action)
	}

	if r.Local.Files == 0 {
		fmt.Fprintf(w, "%s%s\n", label("local"), paint(w, styleDim, "none"))
	} else {
		list := strings.Join(r.Local.Paths, " · ")
		if len(r.Local.Paths) < r.Local.Files {
			list += " · …"
		}
		if r.Local.Bytes > 0 {
			list += " (" + humanBytes(r.Local.Bytes) + ")"
		}
		fmt.Fprintf(w, "%s%s%s — stay here\n", label("local"), column(counts[1]), list)
	}
	if r.Hidden.Files == 0 {
		fmt.Fprintf(w, "%s%s\n", label("hidden"), paint(w, styleDim, "none"))
	} else {
		fmt.Fprintf(w, "%s%s\n", label("hidden"), counts[2])
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "%s%s\n", label("warn"), paint(w, styleYellow, warning))
	}
	fmt.Fprintf(w, "%s%s\n", label("lock"), r.Lock)
}

// gitSummary renders one `git · origin … · HEAD …` fragment per repo, or "no git".
func gitSummary(repos []api.PlanRepo) string {
	if len(repos) == 0 {
		return "no git"
	}
	fragments := make([]string, len(repos))
	for i, repo := range repos {
		parts := []string{strings.TrimSpace("git " + repo.Path)}
		if repo.Origin != "" {
			parts = append(parts, "origin "+shortOrigin(repo.Origin))
		} else {
			parts = append(parts, "no origin")
		}
		if repo.Head != "" {
			head := "HEAD " + abbrev(repo.Head, 7)
			if repo.Pushed {
				head += " (pushed)"
			} else {
				head += " (not pushed)"
			}
			parts = append(parts, head)
		}
		fragments[i] = strings.Join(parts, " · ")
	}
	return strings.Join(fragments, "   ")
}

// shortOrigin strips scheme, user and .git from a remote URL so it reads as host/owner/repo.
func shortOrigin(url string) string {
	s := url
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else if at := strings.Index(s, "@"); at >= 0 && strings.Contains(s[at:], ":") {
		s = strings.Replace(s[at+1:], ":", "/", 1) // scp-like git@host:owner/repo
	}
	if at := strings.Index(s, "@"); at >= 0 && at < strings.Index(s+"/", "/") {
		s = s[at+1:]
	}
	return strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
}

// pathSample lists up to n of a set's paths, with an ellipsis when more exist.
func pathSample(set api.PlanSet, n int) string {
	if len(set.Paths) == 0 {
		return ""
	}
	shown := set.Paths[:min(n, len(set.Paths))]
	list := strings.Join(shown, ", ")
	if len(shown) < set.Files {
		list += ", …"
	}
	return "  (" + list + ")"
}

// humanBytes is the shared base-1000 size rendering: B, KB, MB, GB, TB with at most one decimal.
func humanBytes(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10) + " B"
	}
	v := float64(n)
	unit := ""
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		v /= 1000
		unit = u
		if v < 999.95 {
			break
		}
	}
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + " " + unit
}

// plural renders a count with the singular or plural noun, e.g. "1 file", "1,024 files".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return commas(int64(n)) + " " + many
}

// commas renders an integer with thousands separators.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// abbrev shortens an id to its first n characters.
func abbrev(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// orDefault returns s, or fallback when s is empty.
func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// stamp renders a time for tables, local and to the minute; "-" for none, as
// from a daemon older than the field.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// yesNo renders a flag for tables.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
