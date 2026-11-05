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
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
)

// cli carries the resolved options and the lazily dialed daemon connection of one invocation.
type cli struct {
	ctx       context.Context
	state     string
	mount     string
	json      bool
	autostart bool // start the daemon when its socket does not accept connections
	cmd       command
	conn      *admin.Client
}

// command is one CLI verb: its usage, help line and implementation.
type command struct {
	name      string
	args      string
	desc      string
	autostart bool
	run       func(c *cli, args []string) error
}

// commands returns the verb table; a function so help can refer to it without an init cycle.
func commands() []command {
	return []command{
		{"status", "", "show the running daemon", false, runStatus},
		{"ls", "[CHECKOUT]", "list checkouts, or one checkout's snapshots", true, runLs},
		{"new", "[DIR] [--name N] [--rev REV | --from SNAP] [--take SRC:PATH]... [--rule LINE]...", "open a checkout on a folder (default .), git revision, or snapshot", true, runNew},
		{"run", "[DIR] [--rev REV] [--take SRC:PATH]... [--keep] [--on HOST] -- CMD...", "run a command in a disposable checkout (also --name, --rule, --remote-bin)", true, runRun},
		{"fork", "CHECKOUT [--name N] [-m LABEL]", "snapshot a checkout and open a second checkout on it", true, runFork},
		{"snap", "CHECKOUT [-m LABEL] [--wait]", "freeze a checkout's changes into a snapshot", true, runSnap},
		{"diff", "CHECKOUT [SNAP] [-p|--patch]", "list source changes since a snapshot (default: the base)", true, runDiff},
		{"grep", "PATTERN [DIR|CHECKOUT] [--glob GLOB] [-F] [-i] [-C N] [--max-count N]", "search contents in the daemon, bypassing the checkout mount", true, runGrep},
		{"glob", "PATTERN [DIR|CHECKOUT] [--type file|dir|symlink] [--limit N] [--mtime]", "list matching paths in the daemon, bypassing the checkout mount", true, runGlob},
		{"apply", "CHECKOUT [--dry-run] [--branch B]", "merge a checkout's source changes back into its folder", true, runApply},
		{"rm", "CHECKOUT", "remove a checkout; its snapshots stay", true, runRm},
		{"drop", "SNAP", "release a snapshot", true, runDrop},
		{"explain", "[DIR|CHECKOUT] PATH [--rule LINE]...", "show which class a path gets and which rule decided", true, runExplain},
		{"plan", "[DIR|CHECKOUT] [--on cloud|peer] [--rule LINE]...", "show what a launch elsewhere would transfer, rebuild and leave", true, runPlan},
		{"attach", "[DIR|CHECKOUT] --to MEMBER|HOST [--name N] [--remote-bin PATH]", "make a checkout appear on another machine (a mesh member, else over ssh); content moves on first read", true, runAttach},
		{"pull", "CHECKOUT [--from HOST] [--dry-run]", "merge attached remote source changes into the local checkout", true, runPull},
		{"share", "CHECKOUT [--rw]", "serve a checkout over remote-fs for a VM (vmon --remote-fs TAG:SOCK[:rw]) or a peer", true, runShare},
		{"mesh", "setup|join TOKEN|ls|rm MEMBER|leave", "join machines into a mesh; attach --to a member then runs over it", true, runMesh},
		{"link", "", "relay an attach session from ssh into this machine's daemon (run by attach)", true, runLink},
		{"subscribe", "", "stream daemon events as JSON lines until interrupted", true, runSubscribe},
		{"stop", "", "stop the daemon", false, runStop},
		{"start", "[--json]", "make sure the daemon runs (starting it in the background); --json prints its status and socket", false, runStart},
		{"ctl", "OP ['JSON']", "send one raw admin request and print the raw reply", true, runCtl},
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
		fmt.Printf("usage: ovnfs %s %s\n  %s\n", c.cmd.name, c.cmd.args, c.cmd.desc)
		return nil, exitCode(0)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", c.cmd.name, err, c.usage())
	}
	if len(pos) < minPos || len(pos) > maxPos {
		return nil, errors.New(c.usage())
	}
	return pos, nil
}

// usage is the one-line synopsis of the running verb.
func (c *cli) usage() string {
	return strings.TrimSpace("usage: ovnfs " + c.cmd.name + " " + c.cmd.args)
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
	var reply T
	raw, err := c.call(op, args)
	if err != nil {
		return reply, err
	}
	if c.json {
		fmt.Println(string(raw))
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return reply, fmt.Errorf("%s: decoding reply: %w", op, err)
	}
	return reply, nil
}

// runLs lists checkouts, or the snapshots of one checkout.
func runLs(c *cli, args []string) error {
	pos, err := c.parse(c.flags("ls"), args, 0, 1)
	if err != nil {
		return err
	}
	r, err := call[api.LsReply](c, "ls", struct{}{})
	if err != nil || c.json {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	if len(pos) == 0 {
		if len(r.Checkouts) == 0 {
			return nil
		}
		snaps := make(map[string]int)
		for _, s := range r.Snapshots {
			snaps[s.Checkout]++
		}
		fmt.Fprintln(tw, "CHECKOUT\tSOURCE\tSNAPS\tCHANGED\tPATH")
		for _, h := range r.Checkouts {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", h.Name, h.Source, snaps[h.Name], yesNo(h.Dirty), h.Path)
		}
		return nil
	}
	header := false
	for _, s := range r.Snapshots {
		if s.Checkout != pos[0] {
			continue
		}
		if !header {
			fmt.Fprintln(tw, "ID\tLABEL\tCREATED\tTREE")
			header = true
		}
		tree := "hashing…"
		if s.Tree != "" {
			tree = abbrev(s.Tree, 12)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, s.Label, s.Created.Local().Format("2006-01-02 15:04"), tree)
	}
	return nil
}

// runNew opens a checkout on a folder or a snapshot and prints its path.
func runNew(c *cli, args []string) error {
	fs := c.flags("new")
	var a api.NewArgs
	fs.StringVar(&a.Name, "name", "", "checkout name")
	fs.StringVar(&a.Snapshot, "from", "", "open on this snapshot instead of a folder")
	fs.StringVar(&a.Rev, "rev", "", "start at this git commit or local branch; keep ignored files")
	fs.Var((*stringList)(&a.Take), "take", "replace source path from working tree (.), index (empty), or revision (repeatable)")
	fs.Var((*stringList)(&a.Rules), "rule", "extra rule line (repeatable)")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if a.Snapshot != "" {
		if a.Rev != "" {
			return fmt.Errorf("new: --rev and --from cannot be combined")
		}
		if len(a.Take) != 0 {
			return fmt.Errorf("new: --take and --from cannot be combined")
		}
		if len(pos) != 0 {
			return fmt.Errorf("new: give either DIR or --from SNAP, not both")
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
	fmt.Println(r.Checkout.Path)
	from := r.Checkout.Source
	if a.Snapshot != "" {
		from = strings.TrimPrefix(from+"@", "@") + a.Snapshot
	}
	fmt.Fprintf(os.Stderr, "%s  ←  %s\n", r.Checkout.Name, from)
	if cp := r.Capture; cp != nil {
		fmt.Fprintf(os.Stderr, "  source %s files (%s) · hashed %s (%s) in %.1fs\n",
			commas(int64(cp.Files)), humanBytes(cp.Bytes), commas(int64(cp.Hashed)), humanBytes(cp.HashedBytes), cp.Seconds)
		git := "none"
		if len(cp.Repos) > 0 {
			repos := make([]string, len(cp.Repos))
			for i, r := range cp.Repos {
				repos[i] = orDefault(r, ".")
			}
			git = strings.Join(repos, ", ")
		}
		fmt.Fprintf(os.Stderr, "  cache %s · local %s · hidden %s · git: %s\n",
			commas(int64(cp.Cache)), commas(int64(cp.Local)), commas(int64(cp.Hidden)), git)
	}
	for _, w := range r.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
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
	fmt.Println(r.Checkout.Path)
	fmt.Fprintf(os.Stderr, "%s  ←  %s@%s\n", r.Checkout.Name, a.Checkout, r.Snapshot.ID)
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
	fmt.Println(r.Snapshot.ID)
	line := fmt.Sprintf("snapshot %s of %s", r.Snapshot.ID, a.Checkout)
	if r.Snapshot.Tree != "" {
		line += " tree " + abbrev(r.Snapshot.Tree, 12)
	}
	fmt.Fprintln(os.Stderr, line)
	return nil
}

// runDiff prints a checkout's changed source paths and, on request, the patch.
func runDiff(c *cli, args []string) error {
	fs := c.flags("diff")
	var a api.DiffArgs
	fs.BoolVar(&a.Patch, "p", false, "print a unified diff")
	fs.BoolVar(&a.Patch, "patch", false, "print a unified diff")
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
		fmt.Printf("%s\t%s\n", ch.Status, ch.Path)
	}
	if r.Patch != "" {
		fmt.Print(r.Patch)
		if !strings.HasSuffix(r.Patch, "\n") {
			fmt.Println()
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
	return printMerge(c, r, a.DryRun, again)
}

// printMerge prints apply and pull outcomes and fails when conflicts remain,
// asking the user to resolve conflict markers and then run again.
func printMerge(c *cli, r api.ApplyReply, dryRun bool, again string) error {
	if !c.json {
		if dryRun {
			fmt.Fprintln(os.Stderr, "dry run: nothing written")
		}
		for _, ch := range r.Applied {
			if r.Branch != "" {
				fmt.Printf("index %s\t%s\n", ch.Status, ch.Path)
			} else {
				fmt.Printf("%s\t%s\n", ch.Status, ch.Path)
			}
		}
		for _, p := range r.Merged {
			fmt.Printf("merged %s\n", p)
		}
		for _, cf := range r.Conflicts {
			verb := "conflict"
			if cf.Markers {
				verb = "marked"
			}
			fmt.Printf("%s %s: %s\n", verb, cf.Path, cf.Reason)
		}
		if r.Branch != "" {
			fmt.Printf("branch %s at %s\n", r.Branch, abbrev(r.Commit, 12))
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
		fmt.Fprintf(os.Stderr, "%s left untouched\n", plural(untouched, "conflict", "conflicts"))
	}
	switch {
	case marked > 0 && dryRun:
		fmt.Fprintf(os.Stderr, "%s would have conflict markers\n", plural(marked, "file", "files"))
	case marked > 0:
		fmt.Fprintf(os.Stderr, "%s conflict markers: resolve them, then run `%s` again\n", plural(marked, "file has", "files have"), again)
	}
	return exitCode(1)
}

// runRm removes a checkout.
func runRm(c *cli, args []string) error {
	pos, err := c.parse(c.flags("rm"), args, 1, 1)
	if err != nil {
		return err
	}
	_, err = call[json.RawMessage](c, "rm", api.RmArgs{Checkout: pos[0]})
	return err
}

// runAttach offers a folder or checkout to another machine and prints where it appeared.
func runAttach(c *cli, args []string) error {
	fs := c.flags("attach")
	to := fs.String("to", "", "mesh member or ssh destination of the remote machine")
	name := fs.String("name", "", "checkout name on the remote machine")
	bin := fs.String("remote-bin", "", "path of ovnfs on the remote machine (default: ovnfs on its PATH)")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if *to == "" {
		return fmt.Errorf("attach needs --to MEMBER|HOST")
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
	fmt.Printf("%s:%s\n", *to, reply.RemotePath)
	fmt.Fprintf(os.Stderr, "%s@%s  →  %s:%s (content moves on first read; the link reconnects while this daemon runs)\n", reply.Checkout, reply.Snapshot.ID, reply.Host, reply.RemoteCheckout)
	return nil
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
	fmt.Println(reply.Socket)
	mode := ""
	if reply.Writable {
		mode = ":rw"
	}
	fmt.Fprintf(os.Stderr, "vmon --remote-fs home:%s%s\n", reply.Socket, mode)
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
	line := fmt.Sprintf("%-7s  %s  %s %s", r.Class, r.Rule.Origin, r.Rule.Class, r.Rule.Text)
	if r.At != "" && r.At != r.Path {
		line += " (decided at " + r.At + "/)"
	}
	if !r.Exists {
		line += " (no such path; classified by name)"
	}
	fmt.Println(line)
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
	if s == "." || s == ".." || strings.ContainsRune(s, '/') || strings.ContainsRune(s, filepath.Separator) {
		return true
	}
	info, err := os.Stat(s)
	return err == nil && info.IsDir()
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
	fmt.Fprintf(w, "%s → %s     %s\n", r.Root, r.On, gitSummary(r.Repos))
	const indent = "         " // width of the class label column
	label := func(s string) string { return fmt.Sprintf("%-9s", s) }
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
		fmt.Fprintf(w, "%snone\n", label("cache"))
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
		action := "starts empty"
		if cache.Rebuild != "" {
			action = "rebuild: " + cache.Rebuild
		}
		if cache.KeyHash != "" {
			key := cache.KeyHash
			if len(key) > 11 {
				key = key[:11] + "…"
			}
			action += "  key " + key
		}
		fmt.Fprintf(w, "%s%-*s  %s\n", prefix, rootWidth, cache.Root, action)
	}

	if r.Local.Files == 0 {
		fmt.Fprintf(w, "%snone\n", label("local"))
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
		fmt.Fprintf(w, "%snone\n", label("hidden"))
	} else {
		fmt.Fprintf(w, "%s%s\n", label("hidden"), counts[2])
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "%s%s\n", label("warn"), warning)
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

// yesNo renders a flag for tables.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
