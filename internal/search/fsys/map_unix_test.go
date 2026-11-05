//go:build darwin || linux

package fsys

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// mapTemp writes content to a temp file and maps up to limit bytes of it.
func mapTemp(t *testing.T, content []byte, limit int64) (data []byte, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Native.NewSession()
	h, _, err := s.OpenFile(Handle{}, path)
	if err != nil {
		t.Fatal(err)
	}
	data, err = s.Map(h, limit)
	s.Close(h) // the mapping outlives the handle
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Unmap(data) })
	return data, path
}

func TestMapStopsAtLimitAndSize(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789abcdef"), 3<<16) // 3 MiB
	if data, _ := mapTemp(t, content, 1<<20); !bytes.Equal(data, content[:1<<20]) {
		t.Errorf("limit 1 MiB: mapped %d bytes, want the first %d", len(data), 1<<20)
	}
	if data, _ := mapTemp(t, content, 8<<20); !bytes.Equal(data, content) {
		t.Errorf("limit past size: mapped %d bytes, want all %d", len(data), len(content))
	}
}

// A file truncated under its mapping faults on read; Guard must turn that
// into false instead of a crash.
func TestGuardReportsTruncatedMapping(t *testing.T) {
	data, path := mapTemp(t, bytes.Repeat([]byte("x"), 3<<20), 4<<20)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	var sum byte
	if Guard(data, func() {
		for _, b := range data {
			sum += b
		}
	}) {
		t.Errorf("Guard = true reading a mapping past a truncation (sum %d)", sum)
	}
}

func TestGuardPropagatesOtherPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != "boom" {
			t.Errorf("recovered %v, want the read's own panic", r)
		}
	}()
	Guard(nil, func() { panic("boom") })
}
