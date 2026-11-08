package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/source"
)

// runOptions separates ovnfs flags from the command's untouched arguments.
type runOptions struct {
	new       api.NewArgs
	keep      bool
	on        string
	remoteBin string
	command   []string
}

// parseRun requires an explicit separator so command flags never reach ovnfs's parser.
func parseRun(c *cli, args []string) (runOptions, error) {
	var o runOptions
	fs := c.flags("run")
	fs.StringVar(&o.new.Name, "name", "", "checkout name")
	fs.StringVar(&o.new.Rev, "rev", "", "start at this git commit or local branch")
	fs.Var((*stringList)(&o.new.Take), "take", "SRC:PATH override of the starting tree (repeatable)")
	fs.Var((*stringList)(&o.new.Rules), "rule", "extra rule line (repeatable)")
	fs.BoolVar(&o.keep, "keep", false, "keep the checkout after the command exits")
	fs.StringVar(&o.on, "on", "", "run on this ssh destination, keeping both checkouts")
	fs.StringVar(&o.remoteBin, "remote-bin", "", "path of ovnfs on the remote machine")
	before := args
	separator := slices.Index(args, "--")
	if separator >= 0 {
		before, o.command = args[:separator], args[separator+1:]
	}
	pos, err := c.parse(fs, before, 0, 1)
	if err != nil {
		return o, err
	}
	if separator < 0 || len(o.command) == 0 {
		return o, errors.New(c.usage())
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	o.new.Dir, err = filepath.Abs(dir)
	return o, err
}

// runRun opens an isolated checkout, runs a command there, and closes or retains the lane.
func runRun(c *cli, args []string) error {
	o, err := parseRun(c, args)
	if err != nil {
		return err
	}
	// A run's stdout belongs exclusively to its command, even under --json.
	c.json = false
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if o.on != "" {
		return runRemote(c, o, cwd, signals)
	}
	r, err := call[api.NewReply](c, "new", o.new)
	if err != nil {
		return err
	}
	defer func() {
		if o.keep {
			fmt.Fprintf(stderr, "%s %s %s\n", paint(stderr, styleGreen, "kept"), paint(stderr, styleAccent, r.Checkout.Name), paint(stderr, styleDim, r.Checkout.Path))
			return
		}
		// SIGINT/SIGTERM cancel the CLI context, but must not cancel removal.
		c.ctx = context.WithoutCancel(c.ctx)
		if _, err := call[struct{}](c, "rm", api.RmArgs{Checkout: r.Checkout.Name}); err != nil {
			fmt.Fprintf(stderr, "%s %v\n", paint(stderr, styleYellow, "run: removing "+r.Checkout.Name+":"), err)
		}
	}()
	cmd := exec.Command(o.command[0], o.command[1:]...)
	cmd.Dir = filepath.Join(r.Checkout.Path, runRelativeDir(r.Checkout.Source, cwd))
	cmd.Env = append(os.Environ(), "OVNFS_CHECKOUT="+r.Checkout.Name)
	return runChild(cmd, signals)
}

// runRemote retains the local and remote checkouts so changes can later be pulled back.
func runRemote(c *cli, o runOptions, cwd string, signals <-chan os.Signal) error {
	root, err := source.Resolve(o.new.Dir)
	if err != nil {
		return err
	}
	a := api.AttachArgs{Dir: o.new.Dir, To: o.on, Name: o.new.Name, RemoteBin: o.remoteBin}
	if a.Name == "" {
		a.Name = runLaneName(root)
	}
	if o.new.Rev != "" || len(o.new.Take) > 0 || len(o.new.Rules) > 0 {
		r, err := call[api.NewReply](c, "new", o.new)
		if err != nil {
			return err
		}
		a.Dir, a.Checkout, root = "", r.Checkout.Name, r.Checkout.Source
	}
	r, err := call[api.AttachReply](c, "attach", a)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "%s %s %s %s\n", paint(stderr, styleDim, "lane"), paint(stderr, styleAccent, r.Checkout), paint(stderr, styleDim, "→"), paint(stderr, styleAccent, o.on+":"+r.RemotePath))
	remoteDir := path.Join(r.RemotePath, filepath.ToSlash(runRelativeDir(root, cwd)))
	cmd := sshCommand(o.on, runRemoteCommand(remoteDir, o.command))
	return runChild(cmd, signals)
}

// runRelativeDir preserves the caller's subdirectory only when it is inside the source.
func runRelativeDir(root, cwd string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "."
	}
	return rel
}

// runLaneName keeps a folder's remote name stable across fresh captures.
func runLaneName(root string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(filepath.Base(root)) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '.', ch == '_', ch == '-':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if len(name) > 56 {
		name = name[:56]
	}
	if name == "" {
		return "checkout"
	}
	return name
}

// runRemoteCommand protects every argument from the remote login shell.
func runRemoteCommand(dir string, args []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(dir))
	b.WriteString(" && exec")
	for _, arg := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(arg))
	}
	return b.String()
}

// runChild forwards termination signals while preserving the command's exit status.
func runChild(cmd *exec.Cmd, signals <-chan os.Signal) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, colorError(fmt.Errorf("run: %w", err)))
		return exitCode(127)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	if err := cmd.Wait(); err != nil {
		if exited, ok := errors.AsType[*exec.ExitError](err); ok {
			if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return exitCode(128 + int(status.Signal()))
			}
			return exitCode(exited.ExitCode())
		}
		return err
	}
	return nil
}
