package export

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestNodeLogLoadingProgress(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	// Repeated watermarks exercise multi-MB replay without a large live heap.
	payload := bytes.Repeat([]byte{opNext, 99}, (2<<20)/2+1)
	if err := os.WriteFile(name, append(append([]byte{}, nodeLogMagic...), payload...), 0600); err != nil {
		t.Fatal(err)
	}
	last, calls := -1, 0
	nt, err := openNodeTableWithLoading(name, nil, func(done, total int) {
		if total != len(payload) || done < last || done > total {
			t.Fatalf("loading %d/%d after%d", done, total, last)
		}
		last = done
		calls++
	})
	if err != nil {
		t.Fatal(err)
	}
	defer nt.close()
	if calls < 4 || last != len(payload) {
		t.Fatalf("replay progress calls%d last%d", calls, last)
	}
	if nt.next < 99 {
		t.Fatal("progress altered replay")
	}
}
