package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
)

// searchFlags are the traversal options grep and glob share; kong flattens the
// embedded struct into their flags.
type searchFlags struct {
	Path        string `name:"path" help:"search this root-relative subpath"`
	Hidden      bool   `name:"hidden" help:"include dot-prefixed entries"`
	NoIgnore    bool   `name:"no-ignore" help:"do not honor .gitignore/.ignore/exclude files"`
	NodeModules bool   `name:"node-modules" help:"descend into node_modules"`
	Workers     int    `name:"workers" help:"walking goroutines (0: platform default)"`
	MaxDepth    int    `name:"max-depth" help:"maximum entry depth, with root children at 1 (0: unlimited)"`
}

// searchArgs resolves target as the search root and normalizes the subpath.
func (s searchFlags) searchArgs(target string) (api.SearchArgs, error) {
	a := api.SearchArgs{Path: s.Path, Hidden: s.Hidden, NoIgnore: s.NoIgnore, NodeModules: s.NodeModules, Workers: s.Workers, MaxDepth: s.MaxDepth}
	var err error
	if a.Dir, a.Checkout, err = resolveTarget(target); err != nil {
		return a, err
	}
	if a.Path != "" {
		a.Path, err = relativeTo(a.Dir, a.Path)
	}
	return a, err
}

// grepCmd searches file contents and prints matches, matching paths, or counts.
type grepCmd struct {
	Pattern         string   `arg:"" name:"PATTERN" help:"regular expression, or literal text with -F"`
	Target          string   `arg:"" optional:"" name:"DIR|CHECKOUT" help:"folder or checkout to search (default .)" completion-predictor:"checkout"`
	Literal         bool     `name:"literal" short:"F" help:"search PATTERN as literal text"`
	IgnoreCase      bool     `name:"ignore-case" short:"i" help:"ignore case (Unicode simple folding)"`
	Multiline       bool     `name:"multiline" help:"let matches span lines"`
	Glob            string   `name:"glob" help:"search only files whose relative path matches GLOB, or under a matching directory"`
	Anchored        bool     `name:"anchored" help:"match slash-free globs at the root only"`
	Exclude         []string `name:"exclude" sep:"none" help:"skip files and directories matching GLOB (repeatable; a leading / anchors to the root)"`
	Type            string   `name:"type" help:"search only files of TYPE (rust, ts, py, ... or an extension)"`
	Mode            string   `name:"mode" default:"content" enum:"content,filesWithMatches,count" help:"report content, filesWithMatches, or count"`
	Before          int      `name:"before" short:"B" default:"-1" help:"context lines before each match (overrides -C)"`
	After           int      `name:"after" short:"A" default:"-1" help:"context lines after each match (overrides -C)"`
	Around          int      `name:"context" short:"C" default:"0" help:"context lines before and after each match unless -B or -A is set"`
	MaxColumns      int      `name:"max-columns" help:"cut reported lines longer than N bytes (0: never)"`
	Offset          int      `name:"offset" help:"skip the first N results"`
	MaxCount        int      `name:"max-count" help:"return at most N results (0: all)"`
	MaxCountPerFile int      `name:"max-count-per-file" help:"keep at most N content matches per file (0: all)"`
	MaxFileSize     int64    `name:"max-file-size" help:"search at most N leading bytes per file (0: 4 MiB)"`
	searchFlags
}

func (cmd *grepCmd) Run(c *cli) error {
	search, err := cmd.searchFlags.searchArgs(cmd.Target)
	if err != nil {
		return fmt.Errorf("grep: %w", err)
	}
	a := api.GrepArgs{
		SearchArgs: search, Pattern: cmd.Pattern, Literal: cmd.Literal, IgnoreCase: cmd.IgnoreCase,
		Multiline: cmd.Multiline, Glob: cmd.Glob, Anchored: cmd.Anchored, Exclude: cmd.Exclude,
		Type: cmd.Type, Mode: cmd.Mode, MaxColumns: cmd.MaxColumns, Offset: cmd.Offset,
		MaxCount: cmd.MaxCount, MaxCountPerFile: cmd.MaxCountPerFile, MaxFileSize: cmd.MaxFileSize,
	}
	// Retain gogrep's legacy rule: specifying either side replaces both -C sides.
	a.Before, a.After = cmd.Around, cmd.Around
	if cmd.Before >= 0 || cmd.After >= 0 {
		a.Before, a.After = max(0, cmd.Before), max(0, cmd.After)
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

// globCmd lists matching paths, marking directories with a trailing slash.
type globCmd struct {
	Pattern     string `arg:"" name:"PATTERN" help:"glob pattern"`
	Target      string `arg:"" optional:"" name:"DIR|CHECKOUT" help:"folder or checkout to search (default .)" completion-predictor:"checkout"`
	Anchored    bool   `name:"anchored" help:"match slash-free patterns at the root only"`
	Type        string `name:"type" default:"" enum:"file,dir,symlink" help:"keep only file, dir, or symlink entries"`
	Limit       int    `name:"limit" help:"return at most N matches (0: all)"`
	SortByMtime bool   `name:"mtime" help:"newest first, with size and mtime"`
	searchFlags
}

func (cmd *globCmd) Run(c *cli) error {
	search, err := cmd.searchFlags.searchArgs(cmd.Target)
	if err != nil {
		return fmt.Errorf("glob: %w", err)
	}
	a := api.GlobArgs{
		SearchArgs: search, Pattern: cmd.Pattern, Anchored: cmd.Anchored,
		Type: cmd.Type, Limit: cmd.Limit, SortByMtime: cmd.SortByMtime,
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
