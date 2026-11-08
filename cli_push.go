package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/stencil-hq/ovnfs/api"
)

// runPush has the machines checkouts are attached to fetch all the content
// they still read from this one, drawing a progress bar when stderr is a
// terminal, and fails when any attachment could not.
func runPush(c *cli, args []string) error {
	fs := c.flags("push")
	var a api.PushArgs
	fs.StringVar(&a.To, "to", "", "only the attachments to this host, as passed to attach")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		a.Checkout = pos[0]
	}
	var bar *pushBar
	if isTerminal(os.Stderr) {
		if bar, err = watchPush(c, a, os.Stderr); err != nil {
			return err
		}
	}
	r, err := call[api.PushReply](c, "push", a)
	bar.stop()
	if err != nil {
		return err
	}
	rows := [][]string{}
	failed := 0
	for _, p := range r.Pushed {
		if p.Error != "" {
			failed++
			got := ""
			if p.TotalBlobs > 0 {
				got = fmt.Sprintf(" (%s of %s fetched)", commas(p.Blobs), plural(int(p.TotalBlobs), "blob", "blobs"))
			}
			fmt.Fprintf(stderr, "%s %s: %s%s\n", paint(stderr, styleRed, "push "+p.Checkout), paint(stderr, styleDim, "to "+p.To), paint(stderr, styleRed, p.Error), got)
		} else if !c.json {
			rows = append(rows, []string{paint(stdout, styleAccent, p.Checkout), paint(stdout, styleDim, "→ "+p.To), plural(int(p.Blobs), "blob", "blobs"), humanBytes(p.Bytes)})
		}
	}
	table(stdout, rows)
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

// isTerminal reports whether f is a character device, such as a terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// pushBarWidth is the number of cells in the progress bar; with the figures
// beside it, the line stays within 80 columns.
const pushBarWidth = 16

// pushBar draws the progress of the attachments one push covers as a single
// line, redrawn in place from the daemon's "pushing" events.
type pushBar struct {
	w      io.Writer
	args   api.PushArgs
	cancel context.CancelFunc
	done   chan struct{}
	pushes map[[2]string]api.Pushed // latest progress by checkout and host
	drawn  int                      // runes of the line on screen
}

// watchPush subscribes to the daemon's events and draws the progress of
// pushes a's filters match on w until stop. It subscribes before the push
// starts so no report is missed.
func watchPush(c *cli, a api.PushArgs, w io.Writer) (*pushBar, error) {
	client, err := c.client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(c.ctx)
	events, err := client.Subscribe(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	b := &pushBar{w: w, args: a, cancel: cancel, done: make(chan struct{}), pushes: map[[2]string]api.Pushed{}}
	go func() {
		defer close(b.done)
		for raw := range events {
			var e struct {
				Pushing *api.Pushed `json:"pushing"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Pushing != nil {
				b.update(*e.Pushing)
			}
		}
	}()
	return b, nil
}

// update records one attachment's progress and redraws the line.
func (b *pushBar) update(p api.Pushed) {
	if (b.args.Checkout != "" && p.Checkout != b.args.Checkout) || (b.args.To != "" && p.To != b.args.To) {
		return
	}
	b.pushes[[2]string{p.Checkout, p.To}] = p
	var sum api.Pushed
	for _, p := range b.pushes {
		sum.Blobs += p.Blobs
		sum.Bytes += p.Bytes
		sum.TotalBlobs += p.TotalBlobs
		sum.TotalBytes += p.TotalBytes
	}
	if sum.TotalBlobs == 0 {
		return
	}
	// Bytes say how long is left better than blobs do, unless every blob is empty.
	done, total := sum.Bytes, sum.TotalBytes
	if total == 0 {
		done, total = sum.Blobs, sum.TotalBlobs
	}
	done = min(done, total)
	cells := int(pushBarWidth * done / total)
	line := fmt.Sprintf("push %s%s %3d%%  %s/%s  %s/%s blobs",
		strings.Repeat("█", cells), strings.Repeat("░", pushBarWidth-cells), 100*done/total,
		humanBytes(sum.Bytes), humanBytes(sum.TotalBytes), commas(sum.Blobs), commas(sum.TotalBlobs))
	if n := len(b.pushes); n > 1 {
		line += fmt.Sprintf(" · %d links", n)
	}
	b.draw(line)
}

// draw replaces the line on screen with line.
func (b *pushBar) draw(line string) {
	n := utf8.RuneCountInString(line)
	fmt.Fprintf(b.w, "\r%s%s", line, strings.Repeat(" ", max(0, b.drawn-n)))
	b.drawn = n
}

// stop ends the subscription and erases the line; a nil bar does nothing.
func (b *pushBar) stop() {
	if b == nil {
		return
	}
	b.cancel()
	<-b.done
	if b.drawn > 0 {
		fmt.Fprintf(b.w, "\r%s\r", strings.Repeat(" ", b.drawn))
	}
}
