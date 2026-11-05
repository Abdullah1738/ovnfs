package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/search/grep"
	"github.com/stencil-hq/ovnfs/store"
)

var needle = grep.Query{Pattern: `needle \d+`, Content: true, Before: 1}

// TestSearchScansAtTheSource checks that a search too large for one request
// frame returns one outcome per want, in order, equal to scanning the bytes
// locally; that a blob the source cannot produce comes back unsearched; and
// that nothing a search reads enters the remote's store.
func TestSearchScansAtTheSource(t *testing.T) {
	blobs := newBlobs()
	l := pipeLink(t, blobs)
	local, err := grep.NewMatcher(needle)
	if err != nil {
		t.Fatal(err)
	}
	hint := strings.Repeat("p", 4<<10) // 300 hints need three request frames
	var wants []Want
	var want []grep.Outcome
	for i := range 300 {
		data := fmt.Appendf(nil, "line %d\nneedle %d\nmiss\n", i, i)
		if i%3 == 0 {
			data = fmt.Appendf(nil, "nothing %d\n", i)
		}
		wants = append(wants, Want{ID: blobs.add(data), Path: fmt.Sprintf("%s/%d", hint, i)})
		want = append(want, local.Scan(t.Context(), data))
	}
	wants = slices.Insert(wants, 150, Want{ID: store.HashBytes("blob", []byte("absent")), Path: "absent"})
	want = slices.Insert(want, 150, grep.Outcome{})

	got, err := l.s.Search(t.Context(), needle, wants)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("outcomes differ from a local scan:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	for _, w := range wants {
		if l.st.Has(w.ID) {
			t.Fatalf("searched blob %s entered the store", w.Path)
		}
	}
}

// TestSearchCancelLeavesLinkWorking abandons a search the source is still
// scanning; its late results must not disturb the next search.
func TestSearchCancelLeavesLinkWorking(t *testing.T) {
	blobs := newBlobs()
	l := pipeLink(t, blobs)
	slow := blobs.add([]byte("needle 1\n"))
	release := blobs.hold(slow)
	defer release()
	blobs.mu.Lock()
	blobs.open = make(chan store.OID, 4)
	blobs.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := l.s.Search(ctx, needle, []Want{{ID: slow, Path: "slow"}})
		done <- err
	}()
	wait(t, blobs.open, "the source opening the held blob")
	cancel()
	if err := wait(t, done, "the canceled search"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled search: %v", err)
	}
	release()

	fast := blobs.add([]byte("x\nneedle 2\n"))
	got, err := l.s.Search(t.Context(), needle, []Want{{ID: fast, Path: "fast"}, {ID: slow, Path: "slow"}})
	if err != nil || len(got) != 2 || got[0].Count != 1 || got[0].Matches[0].Line != 2 || got[1].Count != 1 {
		t.Fatalf("search after a canceled one: %+v, %v", got, err)
	}
}

// TestSearchFailsWhenLinkEnds checks that a pending search fails instead of
// waiting when the source closes the link.
func TestSearchFailsWhenLinkEnds(t *testing.T) {
	blobs := newBlobs()
	l := pipeLink(t, blobs)
	id := blobs.add([]byte("needle 1\n"))
	release := blobs.hold(id)
	defer release()
	blobs.mu.Lock()
	blobs.open = make(chan store.OID, 1)
	blobs.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := l.s.Search(context.Background(), needle, []Want{{ID: id, Path: "held"}})
		done <- err
	}()
	wait(t, blobs.open, "the source opening the held blob")
	l.cancel()
	if err := wait(t, done, "the search on a closed link"); err == nil {
		t.Fatal("search succeeded on a closed link")
	}
}
