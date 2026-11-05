package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/store"
)

// convertAttrs are the gitattributes under which git may convert content between
// blob and work tree, in the order classify reads them.
var convertAttrs = []string{"text", "eol", "crlf", "filter", "ident", "working-tree-encoding"}

// conversion is what git may do to a path's bytes between its blob and the work tree.
type conversion int

const (
	convertNone conversion = iota // the blob is the work-tree bytes
	convertEOL                    // end-of-line normalization only, which changes the size iff it changes bytes
	convertAny                    // filter driver, ident or encoding: only reading the file tells
)

// classify decides a path's conversion from its convertAttrs values, as git's
// convert.c does; unknown values count as the conversion they might request.
func classify(values []string, c git.Conversion) conversion {
	text, eol, crlf, filter, ident, encoding := values[0], values[1], values[2], values[3], values[4], values[5]
	if c.Filters[filter] || ident == git.AttrSet || isAttrValue(encoding) {
		return convertAny
	}
	switch text {
	case git.AttrUnset:
		return convertNone
	case git.AttrUnspecified:
		switch crlf {
		case git.AttrUnset:
			return convertNone
		case git.AttrUnspecified:
			if eol == git.AttrUnspecified && !c.AutoCRLF {
				return convertNone
			}
		}
	}
	return convertEOL
}

// isAttrValue reports an attribute set to a value rather than a bare state.
func isAttrValue(v string) bool {
	return v != git.AttrUnspecified && v != git.AttrSet && v != git.AttrUnset
}

// candidate is a stat-clean tracked file waiting for proof that its index blob is its content.
type candidate struct {
	oid string
	j   hashJob
}

// verifier proves, for one repository, which stat-clean tracked files' index blob
// ids equal their work-tree bytes, asking git while the walk continues; every
// other candidate is hashed.
type verifier struct {
	w       *walker
	ctx     context.Context
	repo    *git.Repo
	path    string        // the repository's root-relative work tree
	ready   chan struct{} // closed once conv and attrs are set
	conv    git.Conversion
	attrs   *git.Stream[candidate, []string] // nil when git could not be asked
	sizes   *git.Stream[candidate, int64]    // started by the attrs reader on the first end-of-line candidate
	noSizes bool                             // the sizes stream could not start; attrs reader only
	warn    sync.Once
	mu      sync.Mutex // guards taken, apart from the walker's lock the walk needs
	taken   []candidate
}

// verifier returns the verifier of the repository whose work tree is root-relative
// path, starting its git processes in the background so the walk need not wait.
func (w *walker) verifier(ctx context.Context, path string, repo *git.Repo) *verifier {
	if v := w.verifiers[path]; v != nil {
		return v
	}
	v := &verifier{w: w, ctx: ctx, repo: repo, path: path, ready: make(chan struct{})}
	w.verifiers[path] = v
	go func() {
		defer close(v.ready)
		conv, err := repo.Conversion(ctx)
		if err == nil {
			v.conv = conv
			v.attrs, err = git.CheckAttr(ctx, repo, convertAttrs, v.classified)
		}
		if err != nil {
			v.attrs = nil
			v.warnOnce(err)
		}
	}()
	return v
}

// add queues one candidate by its repository-relative path; walk goroutine only.
func (v *verifier) add(rel, oid string, j hashJob) {
	<-v.ready
	if v.attrs == nil {
		v.w.jobs <- j
		return
	}
	v.attrs.Add(rel, candidate{oid: oid, j: j})
}

// classified acts on a candidate's attributes.
func (v *verifier) classified(c candidate, values []string, err error) {
	if err != nil {
		v.fallback(c, err)
		return
	}
	switch classify(values, v.conv) {
	case convertNone:
		v.take(c)
	case convertEOL:
		v.sized(c)
	default:
		v.w.jobs <- c.j
	}
}

// sized asks for an end-of-line candidate's blob size.
func (v *verifier) sized(c candidate) {
	if v.sizes == nil && !v.noSizes {
		var err error
		if v.sizes, err = git.ObjectSizes(v.ctx, v.repo, v.compared); err != nil {
			v.noSizes = true
			v.warnOnce(err)
		}
	}
	if v.sizes == nil {
		v.w.jobs <- c.j
		return
	}
	v.sizes.Add(c.oid, c)
}

// compared takes the index blob when its size equals the file's.
func (v *verifier) compared(c candidate, size int64, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A blob missing locally (a partial clone) is not fetched just to compare sizes.
		v.w.jobs <- c.j
	case err != nil:
		v.fallback(c, err)
	case size == c.j.info.Size():
		v.take(c)
	default:
		v.w.jobs <- c.j
	}
}

// take keeps the index blob id as the candidate's content, recorded once git has answered all.
func (v *verifier) take(c candidate) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.taken = append(v.taken, c)
}

// fallback hashes a candidate git could not judge, unless the capture was cancelled.
func (v *verifier) fallback(c candidate, err error) {
	if v.ctx.Err() != nil {
		return
	}
	v.warnOnce(err)
	v.w.jobs <- c.j
}

// warnOnce reports the first failure to ask git about this repository's content.
func (v *verifier) warnOnce(err error) {
	v.warn.Do(func() {
		where := v.path
		if where == "" {
			where = "."
		}
		v.w.warn(fmt.Sprintf("%s: cannot check git content conversion, tracked files hashed: %v", where, err))
	})
}

// finishVerifiers waits until every candidate is taken or queued for hashing.
func (w *walker) finishVerifiers() {
	for _, v := range w.verifiers {
		if <-v.ready; v.attrs != nil {
			v.attrs.Close()
		}
	}
	for _, v := range w.verifiers {
		if v.sizes != nil {
			v.sizes.Close()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, v := range w.verifiers {
		for _, c := range v.taken {
			w.res.Files[c.j.p] = store.File{Mode: c.j.mode, ID: store.OID(c.oid)}
		}
		v.taken = nil
	}
}
