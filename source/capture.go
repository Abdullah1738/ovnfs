package source

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// maxWarnings bounds the warnings a capture reports; the rest are counted.
const maxWarnings = 20

// Options configures one capture.
type Options struct {
	Root  string       // absolute source root, as returned by Resolve
	Rules rules.Set    // the checkout's resolved rules
	Store *store.Store // nil hashes without storing (plan and explain of a folder)
	Cache string       // stat cache file; "" hashes every file
	// CacheTrees holds the tree a stat cache names when Store is nil (plan and
	// explain read the cache a stored capture wrote); nil then ignores the cache.
	CacheTrees *store.Store
	State      string // directory for index copies; "" reads live indexes
}

// Info is what a capture remembers about one file or symlink, whatever its class.
// A source file whose size, mtime and inode still match is served from the folder.
type Info struct {
	Mtime time.Time `json:"m"`
	Size  int64     `json:"s"`
	Ino   uint64    `json:"i,omitempty"`
}

// infoOf records a path's stat data.
func infoOf(info os.FileInfo, key fileKey, size int64) Info {
	return Info{Mtime: info.ModTime(), Size: size, Ino: key.ino}
}

// Result is the frozen description of a source folder.
type Result struct {
	Root     string                 `json:"root"`
	Tree     store.OID              `json:"tree,omitempty"` // empty for hash-only captures
	Files    map[string]store.File  `json:"-"`              // source leaves: blobs and empty directories
	Info     map[string]Info        `json:"info"`
	Roots    map[string]rules.Class `json:"roots"` // non-source roots, never descended
	Repos    []Repo                 `json:"repos"`
	Stats    api.Capture            `json:"-"`
	Warnings []string               `json:"-"`
	BlobRepo map[string]string      `json:"blob_repo,omitempty"` // taken file -> root-relative object repository
	Domains  *Domains               `json:"-"`
}

// Resolve returns the source root for dir: the repository's top level when dir is
// inside one, else dir itself, with symlinks resolved so identical folders compare equal.
func Resolve(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	if r, err := git.Open(abs); err == nil {
		return r.Top, nil
	}
	return abs, nil
}

// Capture walks o.Root once. Source-class files are stored (or only hashed when
// o.Store is nil); every other class is recorded as a root and never descended.
func Capture(ctx context.Context, o Options) (Result, error) {
	start := time.Now()
	w := &walker{o: o, res: Result{Root: o.Root, Files: map[string]store.File{}, Info: map[string]Info{}, Roots: map[string]rules.Class{}}, cacheOut: map[string]statEntry{}, cacheReady: make(chan struct{}), verifiers: map[string]*verifier{}}
	var epoch string
	if o.Store != nil {
		// No object is removed until Build has made the tree's objects safe from Sweep,
		// so the stat cache's blobs are all stored if no object was removed since it was saved.
		release := o.Store.Hold()
		defer release()
		epoch = o.Store.Epoch()
	}
	// Reading the stat cache's tree overlaps the start of the walk.
	go w.loadCache(epoch)
	defer func() { <-w.cacheReady }()
	var repos []Repo
	if repo, err := git.Open(o.Root); err == nil && repo.Top == o.Root {
		r, err := w.copyRepo(ctx, repo, "")
		if err != nil {
			return Result{}, err
		}
		repos = append(repos, r)
		w.verifier(ctx, "", repo)
		if busy := repo.Busy(); busy != "" {
			w.warn(fmt.Sprintf("repository has an unfinished %s; its index was captured as is", busy))
		}
	}
	domains, err := NewDomains(o.Root, repos)
	if err != nil {
		w.finishVerifiers()
		return Result{}, err
	}
	w.domains = domains
	w.res.Domains = domains
	w.startPool()
	_, walkErr := w.dir(ctx, "")
	if walkErr == nil {
		<-w.cacheReady
		w.resolvePending(ctx)
	}
	w.finishVerifiers()
	w.stopPool()
	if walkErr != nil {
		return Result{}, walkErr
	}
	if o.Store != nil {
		if w.res.Tree, err = o.Store.Build(w.res.Files); err != nil {
			return Result{}, err
		}
		if o.Cache != "" {
			if err := saveStatCache(o.Cache, w.res.Tree, w.res.Files, w.cacheOut, start, epoch); err != nil {
				w.warn("stat cache not saved: " + err.Error())
			}
		}
	}
	w.res.Repos = domains.Repos()
	st := &w.res.Stats
	st.Root = o.Root
	for _, r := range w.res.Repos {
		st.Repos = append(st.Repos, r.Path)
	}
	for _, class := range w.res.Roots {
		switch class {
		case rules.Cache:
			st.Cache++
		case rules.Local:
			st.Local++
		case rules.Hidden:
			st.Hidden++
		}
	}
	st.Seconds = time.Since(start).Seconds()
	if w.dropped > 0 {
		w.res.Warnings = append(w.res.Warnings, strconv.Itoa(w.dropped)+" more warnings")
	}
	return w.res, nil
}

// walker carries one capture's state; maps are guarded by mu once hashing starts.
type walker struct {
	o           Options
	domains     *Domains
	mu          sync.Mutex
	res         Result
	cacheIn     map[string]statEntry // set before cacheReady closes, read-only after
	cacheStored bool                 // every cacheIn blob is in the store
	cacheReady  chan struct{}        // closed once the stat cache is loaded
	pending     []hashJob            // files met before cacheReady, in walk order; walk goroutine only
	cacheOut    map[string]statEntry
	dropped     int
	repos       int                  // index copies taken so far, naming their directories
	verifiers   map[string]*verifier // by repository path; created before and during the walk, read after it
	jobs        chan hashJob
	workers     sync.WaitGroup
}

// hashJob is one regular file whose content must be read.
type hashJob struct {
	p    string
	info os.FileInfo
	key  fileKey
	mode uint32
}

// abs maps a root-relative path to the host path.
func (w *walker) abs(p string) string { return filepath.Join(w.o.Root, filepath.FromSlash(p)) }

// warn records a capture warning, keeping the first maxWarnings.
func (w *walker) warn(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.res.Warnings) < maxWarnings {
		w.res.Warnings = append(w.res.Warnings, msg)
	} else {
		w.dropped++
	}
}

// copyRepo snapshots a repository's HEAD and index (when the capture keeps state).
func (w *walker) copyRepo(ctx context.Context, repo *git.Repo, p string) (Repo, error) {
	head, err := repo.Head(ctx)
	if err != nil {
		return Repo{}, err
	}
	r := Repo{Path: p, Head: head}
	if w.o.State == "" {
		return r, nil
	}
	dir := filepath.Join(w.o.State, "repos", strconv.Itoa(w.repos))
	w.repos++
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Repo{}, err
	}
	r.Index = filepath.Join(dir, "index")
	if err := git.CopyIndex(repo.IndexPath(), r.Index); err != nil {
		if !os.IsNotExist(err) {
			return Repo{}, err
		}
		r.Index = ""
	}
	return r, nil
}

// dir captures one source-class directory and reports whether it holds any source entry.
func (w *walker) dir(ctx context.Context, rel string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	entries, keyOf, err := readDir(w.abs(rel))
	if err != nil {
		return false, err
	}
	if rel != "" && slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == ".git" }) {
		// Before any entry is classified, so they get the nested repository's facts.
		if err := w.nested(ctx, rel); err != nil {
			w.warn(fmt.Sprintf("%s: nested repository ignored: %v", rel, err))
		}
	}
	any := false
	for _, e := range entries {
		name := e.Name()
		p := path.Join(rel, name)
		if name == ".git" {
			// The git adapter owns .git: the root's is replaced by a private git
			// directory; a nested repository's stays as a local copy-on-write root.
			if rel != "" || !w.domains.RootIsRepo() {
				w.root(p, rules.Local, nil, fileKey{})
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			w.warn(fmt.Sprintf("%s: %v", p, err))
			w.root(p, rules.Hidden, nil, fileKey{})
			continue
		}
		key := keyOf(info)
		class := rules.Source
		if rule, ok := w.o.Rules.Match(p, w.domains.Facts(Dir(w.o.Root), p, info)); ok {
			class = rule.Class
		}
		if class != rules.Source {
			w.root(p, class, info, key)
			continue
		}
		mode, ok := store.ModeOf(info.Mode())
		if !ok {
			w.root(p, rules.Hidden, info, key)
			continue
		}
		switch {
		case info.IsDir():
			has, err := w.dir(ctx, p)
			if err != nil {
				if ctx.Err() != nil {
					return false, err
				}
				w.warn(fmt.Sprintf("%s: %v", p, err))
				w.root(p, rules.Hidden, info, key)
				continue
			}
			if !has {
				w.leaf(p, store.File{Mode: store.ModeDir, ID: store.EmptyTree})
			}
		case mode == store.ModeSymlink:
			if err := w.symlink(p, info, key); err != nil {
				w.warn(fmt.Sprintf("%s: %v", p, err))
				w.root(p, rules.Hidden, info, key)
				continue
			}
		default:
			w.file(ctx, hashJob{p: p, info: info, key: key, mode: mode})
		}
		any = true
	}
	return any, nil
}

// nested opens a repository rooted at source directory p, which holds a .git entry.
func (w *walker) nested(ctx context.Context, p string) error {
	repo, err := git.Open(w.abs(p))
	if err != nil {
		return err
	}
	if repo.Top != w.abs(p) {
		return nil
	}
	r, err := w.copyRepo(ctx, repo, p)
	if err != nil {
		return err
	}
	if err := w.domains.Add(r); err != nil {
		return err
	}
	w.verifier(ctx, p, repo)
	return nil
}

// root records a non-source path; its subtree is never read.
func (w *walker) root(p string, class rules.Class, info os.FileInfo, key fileKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.res.Roots[p] = class
	if info != nil && !info.IsDir() {
		w.res.Info[p] = infoOf(info, key, info.Size())
	}
}

// leaf records a source leaf.
func (w *walker) leaf(p string, f store.File) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.res.Files[p] = f
}

// symlink stores a symlink's target as its blob.
func (w *walker) symlink(p string, info os.FileInfo, key fileKey) error {
	target, err := os.Readlink(w.abs(p))
	if err != nil {
		return err
	}
	// Snapshots are git trees: link targets use '/' whatever the host's separator.
	target = filepath.ToSlash(target)
	var id store.OID
	if w.o.Store != nil {
		if id, err = w.o.Store.PutBytes([]byte(target)); err != nil {
			return err
		}
	} else {
		id = store.HashBytes("blob", []byte(target))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.res.Files[p] = store.File{Mode: store.ModeSymlink, ID: id}
	w.res.Info[p] = infoOf(info, key, int64(len(target)))
	w.res.Stats.Files++
	w.res.Stats.Bytes += int64(len(target))
	return nil
}

// loadCache reads the stat cache for the walk and closes cacheReady.
func (w *walker) loadCache(epoch string) {
	defer close(w.cacheReady)
	trees := w.o.Store
	if trees == nil {
		trees = w.o.CacheTrees
	}
	c := loadStatCache(w.o.Cache, trees)
	w.cacheIn = c.Entries
	w.cacheStored = w.o.Store != nil && c.Epoch == epoch
}

// file records a regular source file and resolves its blob id, or keeps it
// pending while the stat cache is still loading.
func (w *walker) file(ctx context.Context, j hashJob) {
	w.mu.Lock()
	w.res.Info[j.p] = infoOf(j.info, j.key, j.info.Size())
	w.res.Stats.Files++
	w.res.Stats.Bytes += j.info.Size()
	w.mu.Unlock()
	select {
	case <-w.cacheReady:
		w.resolvePending(ctx)
		w.resolve(ctx, j)
	default:
		w.pending = append(w.pending, j)
	}
}

// resolvePending resolves the files met before the stat cache was loaded.
func (w *walker) resolvePending(ctx context.Context) {
	for _, j := range w.pending {
		w.resolve(ctx, j)
	}
	w.pending = nil
}

// resolve reuses a known blob id when stat data proves the content unchanged,
// else queues hashing. The stat cache comes first: it holds work-tree content
// ids, so files git converts need no question to git once captured.
func (w *walker) resolve(ctx context.Context, j hashJob) {
	if e, ok := w.cacheIn[j.p]; ok && e.matches(j.info, j.key, j.mode) && (w.o.Store == nil || w.cacheStored || w.o.Store.Has(e.OID)) {
		w.mu.Lock()
		w.res.Files[j.p] = store.File{Mode: j.mode, ID: e.OID}
		w.cacheOut[j.p] = e
		w.mu.Unlock()
		return
	}
	dom := w.domains.For(j.p)
	rel := dom.rel(j.p)
	if e, ok := dom.index[rel]; ok && indexClean(e, j.info, j.key, j.mode, dom.indexAt.Modified) {
		// Tracked and unchanged: the user's repository holds the content unless git converts it.
		w.verifier(ctx, dom.path, dom.repo).add(rel, e.OID, j)
		return
	}
	w.jobs <- j
}

// startPool starts the hashing workers.
func (w *walker) startPool() {
	n := min(runtime.GOMAXPROCS(0), 16)
	w.jobs = make(chan hashJob, n*4)
	for range n {
		w.workers.Go(func() {
			for j := range w.jobs {
				w.hash(j)
			}
		})
	}
}

// stopPool waits for queued hashing.
func (w *walker) stopPool() {
	close(w.jobs)
	w.workers.Wait()
}

// hash stores (or hashes) one file. An unreadable file becomes hidden with a warning.
func (w *walker) hash(j hashJob) {
	var (
		id   store.OID
		size int64
		err  error
	)
	if w.o.Store != nil {
		id, size, err = w.o.Store.PutFile(w.abs(j.p))
	} else {
		id, size, err = store.HashFile(w.abs(j.p))
	}
	if err != nil {
		w.warn(fmt.Sprintf("%s: %v", j.p, err))
		w.mu.Lock()
		w.res.Roots[j.p] = rules.Hidden
		w.res.Stats.Files--
		w.res.Stats.Bytes -= j.info.Size()
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.res.Files[j.p] = store.File{Mode: j.mode, ID: id}
	w.res.Stats.Hashed++
	w.res.Stats.HashedBytes += size
	if size == j.info.Size() {
		w.cacheOut[j.p] = newStatEntry(j.info, j.key, j.mode, id)
	} else {
		// The file changed while it was read: the stored bytes no longer match the
		// stat data, so this path is never served from the folder.
		w.res.Info[j.p] = Info{Mtime: j.info.ModTime(), Size: size}
	}
}

// SortedPaths returns a map's keys in order, for stable reports.
func SortedPaths[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
