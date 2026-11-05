//go:build linux

package watch

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOverflowWatchesUnannouncedDirs rescans after the kernel queue overflowed
// and dropped the record announcing a new directory.
func TestOverflowWatchesUnannouncedDirs(t *testing.T) {
	data, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events")
	if err != nil {
		t.Skip(err)
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || limit > 1<<16 {
		t.Skipf("max_queued_events = %q", data)
	}
	var r recorder
	w := start(t, r.fn)
	dir := tempDir(t)
	add(t, w, dir)
	flood := filepath.Join(dir, "flood")
	mkdir(t, flood)
	r.waitFor(t, flood)
	r.reset()

	// Stall applying while each file queues two records (create, close); the
	// reader may still drain one buffer of up to 2048 before it blocks.
	w.mu.Lock()
	for i := range limit/2 + 2048 {
		write(t, filepath.Join(flood, strconv.Itoa(i)), "")
	}
	late := filepath.Join(dir, "late")
	mkdir(t, late)
	w.mu.Unlock()

	r.waitFor(t, dir)
	write(t, filepath.Join(late, "f"), "")
	r.waitFor(t, filepath.Join(late, "f"))
}
