package index

import (
	"bytes"
	"errors"
	"strconv"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// ResolveUndo is one REUC record: the conflict stages of a path saved when its
// conflict was resolved, so "git checkout -m" can recreate it.
type ResolveUndo struct {
	Path string
	// Mode and ID hold stages 1 to 3 at indexes 0 to 2; a zero Mode is a
	// missing stage.
	Mode [3]object.Mode
	ID   [3]object.ID
}

var errResolveUndo = errors.New("malformed resolve-undo record")

func parseResolveUndo(b []byte, f object.Format) ([]ResolveUndo, error) {
	var out []ResolveUndo
	for len(b) > 0 {
		nul := bytes.IndexByte(b, 0)
		if nul < 0 {
			return nil, errResolveUndo
		}
		r := ResolveUndo{Path: string(b[:nul])}
		b = b[nul+1:]
		for i := range r.Mode {
			nul := bytes.IndexByte(b, 0)
			if nul < 0 {
				return nil, errResolveUndo
			}
			m, err := strconv.ParseUint(string(b[:nul]), 8, 32)
			if err != nil {
				return nil, errResolveUndo
			}
			r.Mode[i] = object.Mode(m)
			b = b[nul+1:]
		}
		for i, m := range r.Mode {
			if m == 0 {
				continue
			}
			if len(b) < f.Size() {
				return nil, errResolveUndo
			}
			r.ID[i], _ = object.IDFromRaw(f, b[:f.Size()])
			b = b[f.Size():]
		}
		out = append(out, r)
	}
	return out, nil
}

func appendResolveUndo(b []byte, records []ResolveUndo) []byte {
	for _, r := range records {
		b = append(b, r.Path...)
		b = append(b, 0)
		for _, m := range r.Mode {
			b = strconv.AppendUint(b, uint64(m), 8)
			b = append(b, 0)
		}
		for i, m := range r.Mode {
			if m != 0 {
				b = append(b, r.ID[i].Raw()...)
			}
		}
	}
	return b
}
