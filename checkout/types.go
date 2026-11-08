// Package checkout manages checkouts: isolated, copy-on-write copies of any folder that
// agents work in, plus their snapshots, forks and the way back to the source.
//
// A checkout is one overlay (upper → frozen layers → base). The base is a capture
// of the source folder classified by the checkout's rules (see package rules): only
// source-class paths are part of a snapshot's identity; cache and local paths
// are served copy-on-write from the folder itself; hidden paths do not exist.
// A checkout of a git repository also gets a private git directory, so git works
// inside it without writing to the user's repository.
//
// Every method on Registry is one admin operation (package api) with the same
// name and argument struct as the CLI verb.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/internal/search/grep"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
	"github.com/stencil-hq/ovnfs/watch"
)

// Error is an admin-visible failure with a stable protocol code.
type Error struct{ Code, Message string }

// Error returns the underlying explanation without a protocol prefix.
func (e *Error) Error() string { return e.Message }

// ErrorCode returns the stable admin protocol error code.
func (e *Error) ErrorCode() string { return e.Code }

// checkoutMeta is the persisted description of a live checkout.
type checkoutMeta struct {
	Name string `json:"name"`
	// Dir names the checkout's directories below uppers and git when they are not
	// named after the checkout: a checkout prepared before its name was known.
	Dir     string    `json:"dir,omitempty"`
	Base    string    `json:"base"`
	Parent  *string   `json:"parent"` // snapshot the checkout was created from
	Head    *string   `json:"head"`   // the checkout's latest snapshot
	Git     bool      `json:"git"`    // the checkout has a private git directory
	Created time.Time `json:"created"`
	// MergeBases maps a merge source (applySource, or pullSource of an attached
	// lane) to a store tree of the incoming leaves its merges left conflict
	// markers for. Later merges from that source take them as those paths' base,
	// so a conflict the user resolved is not merged again.
	MergeBases map[string]string `json:"merge_bases,omitempty"`
	// Copied lists the directories of the upper that the checkout copied up to hold
	// submodule gitfiles, with the metadata they had then; while they keep it, they
	// are infrastructure, not changes (see Registry.upperEmpty).
	Copied map[string]copiedDir `json:"copied,omitempty"`
}

// dir names the checkout's directories below uppers and git.
func (m checkoutMeta) dir() string {
	if m.Dir != "" {
		return m.Dir
	}
	return m.Name
}

// copiedDir is the metadata of a directory copied up to hold a submodule gitfile.
type copiedDir struct {
	Mode  os.FileMode `json:"mode"`
	Mtime time.Time   `json:"mtime"`
}

// snapMeta is the persisted description of a snapshot; Tree is set once hashed.
type snapMeta struct {
	ID       string    `json:"id"`
	Checkout string    `json:"checkout"`
	Dir      string    `json:"dir,omitempty"` // the checkout's checkoutMeta.Dir
	Parent   *string   `json:"parent"`
	Label    string    `json:"label"`
	Created  time.Time `json:"created"`
	Tree     *string   `json:"tree"`
}

// dir names the directories below uppers and git of the checkout snapshotted.
func (m snapMeta) dir() string {
	if m.Dir != "" {
		return m.Dir
	}
	return m.Checkout
}

// Event is emitted only after its corresponding registry state is durable.
type Event struct {
	Name    string
	Payload any
}

// Config connects persistent storage to the export and admin event stream.
type Config struct {
	State, Mount string
	UserHome     string // folder holding the user-wide ~/.o2/ovnfs.yml; "" ignores it
	// Node is this daemon's node id (its mesh key), by which remote machines
	// know its offers; "" means this registry cannot offer checkouts.
	Node string
	// Fetch asks the link currently connected for key (see LinkKey) for blobs a
	// remote checkout needs; nil means remote checkouts can read only what is cached.
	Fetch func(ctx context.Context, key string, wants []base.Want) error
	// Search asks the link currently connected for key to scan blobs a remote
	// checkout has not fetched on the machine holding them; nil makes grep in
	// remote checkouts fetch what it reads.
	Search  func(ctx context.Context, key string, q grep.Query, wants []base.Want) ([]grep.Outcome, error)
	Emit    func(Event)
	Retire  func(name string)
	Changed func(name string, added bool)
	// Memory keeps checkouts' small written files in memory until they spill
	// into their uppers (see overlay.Memory); nil writes every file to disk,
	// which survives a crash.
	Memory *overlay.Memory
	// BlobCache bounds the bytes of small store blobs served from memory; 0 disables it.
	BlobCache int64
	// Invalidate reports checkout paths the daemon changed behind the file
	// protocols (pull, reattach, edits in a source folder's live paths; nil
	// paths: anything in the checkout), so the export recalls clients' caches.
	// It is called without registry locks held.
	Invalidate func(checkout string, paths []string)
}

// Registry owns the live checkout namespace and all retained snapshot objects.
type Registry struct {
	mu           sync.RWMutex
	config       Config
	mounts       []string // config.Mount and its resolved spelling; see underMount
	store        *store.Store
	node         string
	stateLock    *os.File
	checkouts    map[string]*checkoutRecord
	snapshots    map[string]*snapRecord
	lineage      map[string]*snapRecord
	bases        map[string]*baseRecord
	layers       map[string]*layerRecord
	intents      map[string]diskIntent
	reserved     map[string]bool
	nextSnapshot uint64
	proto        string
	port         int
	rootTime     time.Time
	closed       bool
	operations   sync.WaitGroup
	hashes       sync.WaitGroup
	hasher       *hasher
	gcWake       chan struct{}
	noticeWake   chan struct{}
	stop         chan struct{}
	workers      sync.WaitGroup
	noticeMu     sync.Mutex
	notices      []baseNotice
	closeOnce    sync.Once
	closeErr     error
	watcher      *watch.Watcher // nil where folder events are unsupported
	watchWake    chan struct{}
	rootsMu      sync.Mutex
	roots        map[string]string        // rootsMu: folders a create named, by their resolved source root
	resolving    map[string]chan struct{} // rootsMu: folders being resolved, closed when done
	mirror       *mirror                  // git upstream content of attached checkouts
	commits      stateCommits             // durable state commits requested outside r.mu
	writes       stateWrites              // orders state.json writes
}

// checkoutRecord holds one freeze lock for all calls against a live folder.
type checkoutRecord struct {
	meta       checkoutMeta
	base       *baseRecord
	stack      []string
	upper      string
	fs         *overlay.FS
	freeze     sync.RWMutex
	generation atomic.Uint64
	retired    atomic.Bool
	merge      sync.Mutex // serializes apply and pull, which rewrite meta.MergeBases; taken before Registry.mu and freeze
	git        gitCache   // stored files of the private git directory, for the next snapshot
}

// snapRecord retains its full stack until explicitly dropped.
type snapRecord struct {
	meta        snapMeta
	base        *baseRecord
	stack       []string
	layer       string
	stackParent string
	git         store.OID // stored private git directory tree; "" for none or a legacy copy
	done        chan struct{}
	hashing     bool
	announced   bool
	finished    bool
	dropped     bool
	hashErr     error
}

// baseRecord counts checkout, snapshot and active hash references to a base, and
// holds the rules every checkout on it was opened with.
type baseRecord struct {
	view  *base.Base
	lock  manifest.Lock
	rules rules.Set
	refs  int
}

// layerRecord counts checkout, snapshot and active hash references to a layer.
type layerRecord struct {
	id, base string
	layer    *overlay.Layer
	refs     int
	portable bool
	embedded bool // state.json carries the layer's metadata in place of its manifest file
}

// baseNotice carries a base's fault out of its locks before touching registry state.
type baseNotice struct{ base, fault string }

// Session holds a checkout's freeze lock shared for exactly one filesystem call.
type Session struct {
	FS     *overlay.FS
	Gen    uint64
	record *checkoutRecord
	once   sync.Once
}

// Done releases the freeze lock; repeated calls are harmless.
func (s *Session) Done() {
	if s != nil {
		s.once.Do(func() {
			if s.record != nil {
				s.record.freeze.RUnlock()
			}
		})
	}
}

// coded converts filesystem and capture errors to the public error taxonomy.
func coded(err error) error {
	if err == nil {
		return nil
	}
	if public, ok := errors.AsType[*Error](err); ok {
		return public
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "unavailable", Message: err.Error()}
	}
	if errors.Is(err, os.ErrNotExist) {
		return &Error{Code: "not_found", Message: err.Error()}
	}
	return &Error{Code: "io", Message: err.Error()}
}

// failure formats a stable coded error.
func failure(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// checkContext avoids starting a mutation after its caller has cancelled it.
func checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return coded(ctx.Err())
	default:
		return nil
	}
}

// copyString makes returned metadata independent of mutable registry records.
func copyString(s *string) *string {
	if s == nil {
		return nil
	}
	value := *s
	return &value
}

// deref returns a possibly absent string field's value.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
