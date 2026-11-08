package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// stdout and stderr carry every human-readable line. They downsample the styles
// below to the terminal's colour profile and strip them entirely for output that
// is not a terminal (a pipe, a file, NO_COLOR, --color=never), so the plain text
// is what a script sees and what the tests assert on.
var (
	stdout = colorprofile.NewWriter(os.Stdout, os.Environ())
	stderr = colorprofile.NewWriter(os.Stderr, os.Environ())
)

// The styles: ANSI colours only, so every profile down to 16 colours renders
// them, and colourprofile.Writer handles the rest.
var (
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	styleAccent = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	styleGreen  = lipgloss.NewStyle().Foreground(lipgloss.Green)
	styleRed    = lipgloss.NewStyle().Foreground(lipgloss.Red)
	styleYellow = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	styleBlue   = lipgloss.NewStyle().Foreground(lipgloss.Blue)
)

// setColor applies --color to the human-output writers: auto keeps the detected
// profile, always forces colour even into a pipe, never strips it.
func (c *cli) setColor() error {
	switch c.color {
	case "", "auto":
	case "always":
		stdout.Profile, stderr.Profile = colorprofile.ANSI, colorprofile.ANSI
	case "never":
		stdout.Profile, stderr.Profile = colorprofile.ASCII, colorprofile.ASCII
	default:
		return fmt.Errorf("--color %q: want auto, always or never", c.color)
	}
	return nil
}

// colored reports whether w renders ANSI: the process's own stdout or stderr, on
// a terminal that takes colour. Field order matters: a test buffer, a file, and
// --color=never all get plain text.
func colored(w io.Writer) bool {
	cw, ok := w.(*colorprofile.Writer)
	return ok && cw.Profile >= colorprofile.ANSI
}

// paint renders s in style when w takes colour, and returns it unchanged otherwise.
func paint(w io.Writer, style lipgloss.Style, s string) string {
	if !colored(w) {
		return s
	}
	return style.Render(s)
}

// paintIf colors s only when cond holds, keeping the style at the call site.
func paintIf(w io.Writer, cond bool, style lipgloss.Style, s string) string {
	if !cond {
		return s
	}
	return paint(w, style, s)
}

// table prints rows as columns separated by two spaces, padded by each cell's
// visible width so styled cells stay aligned. The first row is the header. With
// colour off it reproduces text/tabwriter's output byte for byte.
func table(w io.Writer, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, 0, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], lipgloss.Width(cell))
		}
	}
	for _, row := range rows {
		var line strings.Builder
		for i, cell := range row {
			line.WriteString(cell)
			if i < len(row)-1 {
				line.WriteString(strings.Repeat(" ", widths[i]-lipgloss.Width(cell)+2))
			}
		}
		fmt.Fprintln(w, line.String())
	}
}

// classStyle colors a path class as `ovnfs explain` and `ovnfs plan` name it.
func classStyle(class string) lipgloss.Style {
	switch class {
	case "source":
		return styleGreen
	case "cache":
		return styleYellow
	case "local":
		return styleBlue
	case "hidden":
		return styleDim
	default:
		return lipgloss.NewStyle()
	}
}

// mark renders a one-character status: A green, M yellow, D red, T magenta, as
// git's short status does, with the colour carrying the meaning.
func mark(w io.Writer, status string) string {
	switch status {
	case "A":
		return paint(w, styleGreen, "A")
	case "M":
		return paint(w, styleYellow, "M")
	case "D":
		return paint(w, styleRed, "D")
	default:
		return paint(w, styleBlue, status)
	}
}

// ok renders a yes/no flag green or dim, for tables.
func ok(w io.Writer, yes bool, yesText, noText string) string {
	if yes {
		return paint(w, styleGreen, yesText)
	}
	return paint(w, styleDim, noText)
}

// colorPatch colors a unified or git patch line by line: additions green,
// deletions red, hunk headers cyan, file and index headers bold, the rest as is.
func colorPatch(w io.Writer, patch string) {
	if !colored(w) {
		fmt.Fprint(w, patch)
		return
	}
	for _, line := range strings.SplitAfter(patch, "\n") {
		if line == "" {
			continue
		}
		fmt.Fprint(w, patchLine(w, strings.TrimSuffix(line, "\n")))
		if strings.HasSuffix(line, "\n") {
			fmt.Fprint(w, "\n")
		}
	}
}

// patchLine styles one patch line by its leading marker.
func patchLine(w io.Writer, line string) string {
	switch {
	case strings.HasPrefix(line, "@@"):
		return paint(w, styleAccent, line)
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return paint(w, styleBold, line)
	case strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"),
		strings.HasPrefix(line, "old mode"), strings.HasPrefix(line, "new mode"),
		strings.HasPrefix(line, "similarity "), strings.HasPrefix(line, "rename "),
		strings.HasPrefix(line, "copy "), strings.HasPrefix(line, "Binary files"):
		return paint(w, styleBold, line)
	case strings.HasPrefix(line, "+"):
		return paint(w, styleGreen, line)
	case strings.HasPrefix(line, "-"):
		return paint(w, styleRed, line)
	case strings.HasPrefix(line, `\`):
		return paint(w, styleDim, line)
	default:
		return line
	}
}

// highlight wraps every occurrence of needle in text red and bold, for grep of a
// fixed string; a writer without colour, or an empty needle, leaves it alone.
func highlight(w io.Writer, text, needle string) string {
	if !colored(w) || needle == "" {
		return text
	}
	var b strings.Builder
	for {
		i := strings.Index(text, needle)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:i])
		b.WriteString(styleRed.Bold(true).Render(needle))
		text = text[i+len(needle):]
	}
}

// colorError wraps err's text in red when the writer takes colour.
func colorError(err error) string {
	return paint(stderr, styleRed, err.Error())
}
