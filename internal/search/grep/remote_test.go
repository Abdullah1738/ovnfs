package grep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// farRemote plays the machine holding the files under far/: it scans them
// with a Matcher after the query and outcomes cross JSON, as on a link.
type farRemote struct {
	files   map[string]string // walked path → content
	down    bool              // every Scan fails
	scanned atomic.Int64
}

func (r *farRemote) Locate(p string) (Blob, bool) {
	data, ok := r.files[p]
	p = strings.Clone(p)
	return Blob{ID: p, Path: p, Size: int64(len(data))}, ok
}

func (r *farRemote) Scan(ctx context.Context, q Query, blobs []Blob) ([]Outcome, error) {
	if r.down {
		return nil, errors.New("link down")
	}
	wire, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	var got Query
	if err := json.Unmarshal(wire, &got); err != nil {
		return nil, err
	}
	m, err := NewMatcher(got)
	if err != nil {
		return nil, err
	}
	outs := make([]Outcome, len(blobs))
	for i, b := range blobs {
		outs[i] = m.Scan(ctx, []byte(r.files[b.ID]))
	}
	r.scanned.Add(int64(len(blobs)))
	if wire, err = json.Marshal(outs); err != nil {
		return nil, err
	}
	outs = nil
	return outs, json.Unmarshal(wire, &outs)
}

// TestRemoteScansMatchLocal checks that files scanned by a Remote report
// exactly what reading them here reports, merged into path order with the
// local files, under every mode and limit, and when the remote fails.
func TestRemoteScansMatchLocal(t *testing.T) {
	files := map[string]string{
		"near/a.txt":     "hit one\nmiss\nhit two\n",
		"near/big.txt":   strings.Repeat("filler\n", 40) + "hit late\n",
		"far/binary.bin": "hit\x00\n",
		"far/big.txt":    strings.Repeat("filler\n", 40) + "hit late\n",
		"far/long.txt":   strings.Repeat("x", 300) + "hit" + strings.Repeat("y", 300) + "\n",
		"far/block.txt":  "head\nhit a\nhit b\ntail\nhit c\n",
	}
	far := map[string]string{}
	for i := range 1300 {
		files[fmt.Sprintf("far/many/f%04d.txt", i)] = fmt.Sprintf("line %d\nhit %d\n", i, i)
	}
	for name, data := range files {
		if strings.HasPrefix(name, "far/") {
			far["/"+name] = data
		}
	}
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"content with context", Options{Pattern: "hit", Before: 1, After: 1}},
		{"columns", Options{Pattern: "hit", MaxColumns: 40}},
		{"count", Options{Pattern: "hit", Mode: Count}},
		{"files", Options{Pattern: `hit \d`, Mode: FilesWithMatches}},
		{"window", Options{Pattern: "hit", MaxFileSize: 100}},
		{"per file", Options{Pattern: "hit", MaxCountPerFile: 1}},
		{"multiline", Options{Pattern: `hit a\nhit`, Multiline: true}},
		{"ordered early stop", Options{Pattern: "hit", Offset: 3, MaxCount: 5}},
		{"ordered past batches", Options{Pattern: "hit", Offset: 900, MaxCount: 2, Mode: Count}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := grepTree(t, files, tc.opts)
			for _, down := range []bool{false, true} {
				r := &farRemote{files: far, down: down}
				opts := tc.opts
				opts.Remote = r
				got := grepTree(t, files, opts)
				if !reflect.DeepEqual(nilContext(got), nilContext(want)) {
					t.Fatalf("remote down=%v:\n got %+v\nwant %+v", down, got, want)
				}
				if !down && r.scanned.Load() == 0 {
					t.Fatal("no file was scanned remotely")
				}
			}
		})
	}

	r := &farRemote{files: far}
	opts := Options{Pattern: "hit", Remote: r, Walk: tree(files)}
	got, err := Grep(t.Context(), "/far/block.txt", opts)
	opts.Remote = nil
	want, _ := Grep(t.Context(), "/far/block.txt", opts)
	if err != nil || !reflect.DeepEqual(nilContext(got), nilContext(want)) || r.scanned.Load() != 1 {
		t.Fatalf("remote file root: %+v, %v; want %+v", got, err, want)
	}
}

// nilContext makes empty context slices nil: JSON does not tell them apart.
func nilContext(res Result) Result {
	for i := range res.Matches {
		m := &res.Matches[i]
		if len(m.Before) == 0 {
			m.Before = nil
		}
		if len(m.After) == 0 {
			m.After = nil
		}
	}
	return res
}
