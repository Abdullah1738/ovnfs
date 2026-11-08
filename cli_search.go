package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
)

// searchFlags adds the traversal options common to grep and glob.
func searchFlags(fs *flag.FlagSet, a *api.SearchArgs) {
	fs.StringVar(&a.Path, "path", "", "search this root-relative subpath")
	fs.BoolVar(&a.Hidden, "hidden", false, "include dot-prefixed entries")
	fs.BoolVar(&a.NoIgnore, "no-ignore", false, "do not honor .gitignore/.ignore/exclude files")
	fs.BoolVar(&a.NodeModules, "node-modules", false, "descend into node_modules")
	fs.IntVar(&a.Workers, "workers", 0, "walking goroutines (0: platform default)")
	fs.IntVar(&a.MaxDepth, "max-depth", 0, "maximum entry depth, with root children at 1 (0: unlimited)")
}

// resolveSearchTarget resolves the optional root and normalizes its subpath.
func resolveSearchTarget(a *api.SearchArgs, pos []string) error {
	target := ""
	if len(pos) == 2 {
		target = pos[1]
	}
	var err error
	if a.Dir, a.Checkout, err = resolveTarget(target); err != nil {
		return err
	}
	if a.Path != "" {
		a.Path, err = relativeTo(a.Dir, a.Path)
	}
	return err
}

// runGrep searches file contents and prints matches, matching paths, or counts.
func runGrep(c *cli, args []string) error {
	fs := c.flags("grep")
	var a api.GrepArgs
	searchFlags(fs, &a.SearchArgs)
	fs.BoolVar(&a.Literal, "F", false, "search PATTERN as literal text")
	fs.BoolVar(&a.IgnoreCase, "i", false, "ignore case (Unicode simple folding)")
	fs.BoolVar(&a.Multiline, "multiline", false, "let matches span lines")
	fs.StringVar(&a.Glob, "glob", "", "search only files whose relative path matches GLOB, or under a matching directory")
	fs.BoolVar(&a.Anchored, "anchored", false, "match slash-free globs at the root only")
	fs.Func("exclude", "skip files and directories matching GLOB (repeatable; a leading / anchors to the root)", func(v string) error {
		a.Exclude = append(a.Exclude, v)
		return nil
	})
	fs.StringVar(&a.Type, "type", "", "search only files of TYPE (rust, ts, py, ... or an extension)")
	fs.StringVar(&a.Mode, "mode", "content", "report content, filesWithMatches, or count")
	before := fs.Int("B", -1, "context lines before each match (overrides -C)")
	after := fs.Int("A", -1, "context lines after each match (overrides -C)")
	around := fs.Int("C", 0, "context lines before and after each match unless -B or -A is set")
	fs.IntVar(&a.MaxColumns, "max-columns", 0, "cut reported lines longer than N bytes (0: never)")
	fs.IntVar(&a.Offset, "offset", 0, "skip the first N results")
	fs.IntVar(&a.MaxCount, "max-count", 0, "return at most N results (0: all)")
	fs.IntVar(&a.MaxCountPerFile, "max-count-per-file", 0, "keep at most N content matches per file (0: all)")
	fs.Int64Var(&a.MaxFileSize, "max-file-size", 0, "search at most N leading bytes per file (0: 4 MiB)")
	pos, err := c.parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	switch a.Mode {
	case "", "content", "filesWithMatches", "count":
	default:
		return fmt.Errorf("grep: -mode %q: want content, filesWithMatches, or count", a.Mode)
	}
	// Retain gogrep's legacy rule: specifying either side replaces both -C sides.
	a.Before, a.After = *around, *around
	if *before >= 0 || *after >= 0 {
		a.Before, a.After = max(0, *before), max(0, *after)
	}
	a.Pattern = pos[0]
	if err := resolveSearchTarget(&a.SearchArgs, pos); err != nil {
		return fmt.Errorf("grep: %w", err)
	}
	r, err := call[api.GrepReply](c, "grep", a)
	if err != nil || c.json {
		return err
	}
	pattern := ""
	if a.Literal && !a.IgnoreCase {
		pattern = a.Pattern
	}
	for _, m := range r.Matches {
		switch a.Mode {
		case "filesWithMatches":
			fmt.Fprintln(stdout, paint(stdout, styleAccent, m.Path))
		case "count":
			fmt.Fprintf(stdout, "%s:%s\n", paint(stdout, styleAccent, m.Path), paint(stdout, styleGreen, strconv.Itoa(m.Count)))
		default:
			for _, line := range m.Before {
				grepLine(stdout, m.Path, line.Line, line.Text, false, pattern)
			}
			for lineNumber, text := m.Line, m.Text; ; lineNumber++ {
				line, rest, more := strings.Cut(text, "\n")
				grepLine(stdout, m.Path, lineNumber, line, true, pattern)
				if !more {
					break
				}
				text = rest
			}
			for _, line := range m.After {
				grepLine(stdout, m.Path, line.Line, line.Text, false, pattern)
			}
		}
	}
	return nil
}

// grepLine prints one `path:line:text` match or `path-line-text` context line,
// colouring the path, the line number and the matched text.
func grepLine(w io.Writer, path string, line int, text string, match bool, pattern string) {
	num, sep := styleDim, "-"
	if match {
		num, sep = styleGreen, ":"
	}
	fmt.Fprintf(w, "%s%s%s%s%s\n", paint(w, styleAccent, path), paint(w, styleDim, sep), paint(w, num, strconv.Itoa(line)), paint(w, styleDim, sep), highlight(w, text, pattern))
}

// runGlob lists matching paths, marking directories with a trailing slash.
func runGlob(c *cli, args []string) error {
	fs := c.flags("glob")
	var a api.GlobArgs
	searchFlags(fs, &a.SearchArgs)
	fs.BoolVar(&a.Anchored, "anchored", false, "match slash-free patterns at the root only")
	fs.StringVar(&a.Type, "type", "", "keep only file, dir, or symlink entries")
	fs.IntVar(&a.Limit, "limit", 0, "return at most N matches (0: all)")
	fs.BoolVar(&a.SortByMtime, "mtime", false, "newest first, with size and mtime")
	pos, err := c.parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	switch a.Type {
	case "", "file", "dir", "symlink":
	default:
		return fmt.Errorf("glob: -type %q: want file, dir, or symlink", a.Type)
	}
	a.Pattern = pos[0]
	if err := resolveSearchTarget(&a.SearchArgs, pos); err != nil {
		return fmt.Errorf("glob: %w", err)
	}
	r, err := call[api.GlobReply](c, "glob", a)
	if err != nil || c.json {
		return err
	}
	for _, m := range r.Matches {
		if m.Type == "dir" {
			fmt.Fprintln(stdout, paint(stdout, styleAccent, strings.TrimSuffix(m.Path, "/")+"/"))
			continue
		}
		fmt.Fprintln(stdout, m.Path)
	}
	return nil
}
