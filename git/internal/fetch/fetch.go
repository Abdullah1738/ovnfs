// Package fetch transfers objects into a repository from another one: a
// local object database copied in-process, a remote upload-pack service
// spoken to over smart HTTP(S), ssh, git://, a local path or a remote
// helper's connection, using protocol v2 with fallback to v0, a dumb HTTP
// server's files, or a remote helper's own fetch. It also maintains the
// shallow file.
//
// It is written from git's documentation (gitprotocol-*, gitcredentials,
// git-config, urls) and the observed behaviour of the git binary, which it
// never runs.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// Options describes a fetch of objects by id.
type Options struct {
	// URL names the remote repository, in any syntax of urls.adoc, before
	// url.<base>.insteadOf rewriting.
	URL string
	// Wants are the objects to fetch, with everything reachable from them.
	Wants []object.ID
	// Depth limits fetched history to that many commits from each want
	// (git fetch --depth); 0 fetches all of it.
	Depth int
	// Shallow is the target's current shallow boundary (its shallow file).
	Shallow []object.ID
	// Haves are the target's ref tips: commits whose history it has, which
	// the server need not send.
	Haves []object.ID
	// Allowed lists the transports that may be used, by the names of
	// git's protocol.<name>.allow: "https", "http" (smart or dumb), "ssh",
	// "git", "file", or a remote helper's ("ext" for ext::..., "foo" for
	// foo::... and foo://...). Any other is refused before anything
	// connects.
	Allowed []string
	// Config supplies url.*, http.*, credential.*, core.sshCommand,
	// core.askPass and ssh.variant; nil is an empty configuration.
	Config *config.Config
	// Env looks up environment variables (GIT_SSH_COMMAND, GIT_ASKPASS,
	// proxies, ...); nil uses the process environment. Child processes get
	// the variables this package consults from it.
	Env func(string) (string, bool)
	// Target receives the objects; GitDir is the repository it belongs to
	// (Target is GitDir/objects), where packs are written and in which
	// remote helpers run.
	Target *object.DB
	GitDir string
	// MaxObjectSize, when > 0, refuses any object larger than this many
	// bytes (declared in a received pack, copied by the local transport, or
	// read by the connectivity check), bounding the memory a hostile source
	// can make a fetch use.
	MaxObjectSize int64
	// OpenLocal opens the repository at a local path (a file:// URL or a
	// plain path), returning its object database and shallow boundary.
	// Fetch closes the database when done.
	OpenLocal func(path string) (*object.DB, []object.ID, error)
}

// Fetch stores in opts.Target every object reachable from opts.Wants,
// stopping at the shallow boundary, then verifies that they are all present.
// The caller applies Result.Shallow and Result.Unshallow to the shallow file
// (UpdateShallow) and updates refs.
func Fetch(ctx context.Context, opts Options) (Result, error) {
	if opts.Target == nil {
		return Result{}, errors.New("fetch: no target object database")
	}
	if len(opts.Wants) == 0 {
		return Result{}, errors.New("fetch: nothing to fetch")
	}
	format := opts.Target.Format()
	for _, ids := range [][]object.ID{opts.Wants, opts.Haves, opts.Shallow} {
		for _, id := range ids {
			if id.Format() != format {
				return Result{}, fmt.Errorf("fetch: %s is not a %s object name", id, format)
			}
		}
	}
	env := environ{lookup: opts.Env}
	if env.lookup == nil {
		env.lookup = os.LookupEnv
	}
	raw := rewriteURL(opts.URL, insteadOfRules(opts.Config))
	ep, err := parseEndpoint(raw, opts.Allowed)
	if err != nil {
		return Result{}, err
	}
	complete := idSet(opts.Haves)
	// Like git's quick fetch: wants already complete need no transfer.
	if opts.Depth == 0 && checkConnected(ctx, opts.Target, opts.Wants, idSet(opts.Shallow), complete, opts.MaxObjectSize) == nil {
		return Result{}, nil
	}

	f := &fetcher{opts: opts, env: env, req: request{
		format:  format,
		wants:   opts.Wants,
		haves:   opts.Haves,
		shallow: opts.Shallow,
		depth:   opts.Depth,
		thin:    true,
	}}
	var res Result
	if ep.helper {
		res, err = f.helper(ctx, ep)
	} else {
		res, err = f.native(ctx, ep)
	}
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, err
	}

	boundary := idSet(opts.Shallow)
	for _, id := range res.Shallow {
		boundary[id] = struct{}{}
	}
	for _, id := range res.Unshallow {
		delete(boundary, id)
	}
	if err := checkConnected(ctx, opts.Target, opts.Wants, boundary, complete, opts.MaxObjectSize); err != nil {
		return Result{}, err
	}
	return res, nil
}

// native fetches over a transport of this package's own.
func (f *fetcher) native(ctx context.Context, ep *endpoint) (res Result, err error) {
	switch ep.transport {
	case "file":
		res, err = f.local(ctx, ep.path)
	case "ssh":
		var pc *processConn
		if pc, err = dialSSH(ctx, f.opts.Config, f.env, ep); err == nil {
			res, err = f.stateful(pc)
		}
	case "git":
		var tc *tcpConn
		if tc, err = dialGit(ctx, ep); err == nil {
			res, err = f.stateful(tc)
		}
	case "http", "https":
		res, err = f.http(ctx, ep)
	default:
		err = fmt.Errorf("fetch: unsupported transport %q", ep.transport)
	}
	return res, err
}

// insteadOfRules collects every url.<base>.insteadOf.
func insteadOfRules(cfg *config.Config) []insteadOf {
	if cfg == nil {
		return nil
	}
	var rules []insteadOf
	for e := range cfg.All() {
		if e.Section() != "url" || e.Name() != "insteadof" {
			continue
		}
		if base, ok := e.Subsection(); ok {
			rules = append(rules, insteadOf{base: base, prefix: e.Value})
		}
	}
	return rules
}

type fetcher struct {
	opts Options
	env  environ
	req  request
	// advShallow is a v0 server's own shallow boundary.
	advShallow []object.ID
}

func (f *fetcher) packDir() string { return filepath.Join(f.opts.GitDir, "objects", "pack") }

// receive indexes a pack into the target, completing a thin pack from the
// target's objects.
func (f *fetcher) receive(pack *string) receiveFunc {
	return func(r io.Reader) error {
		name, _, err := object.IndexPack(r, f.packDir(), f.req.format, object.IndexPackOptions{
			DB:            f.opts.Target,
			FixThin:       true,
			MaxObjectSize: f.opts.MaxObjectSize,
		})
		*pack = name
		return err
	}
}

// result turns a server's shallow update into a Result once the pack is
// stored. Commits of a shallow v0 server's boundary that arrived without
// their parents are shallow for the target too (v0 servers only announce
// them up front). The server is not trusted: "unshallow" is accepted only
// for commits the client sent as shallow whose parents are now present, and
// "shallow" only for commits present now.
func (f *fetcher) result(su shallowUpdate, pack string) (Result, error) {
	db := f.opts.Target
	if err := db.Rescan(); err != nil {
		return Result{}, err
	}
	res := Result{Pack: pack}
	sent := idSet(f.req.shallow)
	seen := map[object.ID]struct{}{}
	for _, id := range su.unshallow {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := sent[id]; !ok {
			return Result{}, fmt.Errorf("fetch: server unshallowed %s, which the client did not report as shallow", id)
		}
		if err := checkParentsPresent(db, id); err != nil {
			return Result{}, fmt.Errorf("fetch: server unshallowed %s: %w", id, err)
		}
		res.Unshallow = append(res.Unshallow, id)
	}
	known := idSet(f.req.shallow)
	for _, id := range su.shallow {
		if _, dup := known[id]; dup {
			continue
		}
		known[id] = struct{}{}
		if kind, _, err := db.Header(id); err != nil || kind != object.Commit {
			return Result{}, fmt.Errorf("fetch: server marked %s shallow, but it is not a commit in the target", id)
		}
		res.Shallow = append(res.Shallow, id)
	}
	if len(f.advShallow) > 0 {
		for _, id := range missingParents(db, f.advShallow) {
			if _, ok := known[id]; !ok {
				known[id] = struct{}{}
				res.Shallow = append(res.Shallow, id)
			}
		}
	}
	return res, nil
}

// local fetches from a repository on this machine without upload-pack.
func (f *fetcher) local(ctx context.Context, path string) (Result, error) {
	if f.opts.OpenLocal == nil {
		return Result{}, fmt.Errorf("fetch: no way to open local repository %s", path)
	}
	src, srcShallow, err := f.opts.OpenLocal(path)
	if err != nil {
		return Result{}, fmt.Errorf("fetch: opening %s: %w", path, err)
	}
	defer src.Close()
	return Local(ctx, src, f.opts.Target, f.opts.Wants, LocalOptions{
		Depth:         f.opts.Depth,
		SourceShallow: srcShallow,
		TargetShallow: f.opts.Shallow,
		PackDir:       f.packDir(),
		MaxObjectSize: f.opts.MaxObjectSize,
	})
}

// streamConn is a bidirectional connection to upload-pack (ssh, git://, a
// remote helper's).
type streamConn interface {
	io.Reader
	io.Writer
	// finish closes the connection; err is the conversation's outcome,
	// returned with any detail the transport has (stderr of ssh).
	finish(err error) error
}

// stateful runs a fetch over a connection that stays open: the
// advertisement, one request, the response.
func (f *fetcher) stateful(conn streamConn) (Result, error) {
	res, err := f.converse(conn)
	return res, conn.finish(err)
}

func (f *fetcher) converse(conn streamConn) (Result, error) {
	p := newPktReader(conn)
	adv, err := f.advertisement(p)
	if err != nil {
		return Result{}, err
	}
	if adv.v2 {
		res, err := f.fetchV2(adv, conn, p)
		if err == nil {
			// An empty request (flush) ends the v2 session.
			conn.Write([]byte("0000"))
		}
		return res, err
	}
	f.advShallow = adv.shallow
	body, mode, err := f.req.encodeV0(adv)
	if err != nil {
		return Result{}, err
	}
	var pack string
	var su shallowUpdate
	// A v0 server answers the shallow part before reading the haves, so
	// write concurrently with reading to never block on a full pipe.
	werr := make(chan error, 1)
	go func() {
		_, err := conn.Write(body)
		werr <- err
	}()
	su, err = readV0Response(p, f.req.format, f.req.depth > 0, f.req.shallowLimit(), mode, f.receive(&pack))
	if err != nil {
		return Result{}, err
	}
	if err := <-werr; err != nil {
		return Result{}, fmt.Errorf("fetch: sending request: %w", err)
	}
	return f.result(su, pack)
}

// stateless runs a fetch over a remote helper's stateless-connect stream:
// protocol v2 only, the capability advertisement then one request and its
// response, with nothing kept between them.
func (f *fetcher) stateless(conn streamConn) (Result, error) {
	res, err := func() (Result, error) {
		p := newPktReader(conn)
		adv, err := f.advertisement(p)
		if err != nil {
			return Result{}, err
		}
		if !adv.v2 {
			return Result{}, fmt.Errorf("%w: protocol v0 advertisement on a stateless-connect stream", errProtocol)
		}
		return f.fetchV2(adv, conn, p)
	}()
	return res, conn.finish(err)
}

// advertisement reads the server's advertisement and refuses a server that
// cannot serve the request.
func (f *fetcher) advertisement(p *pktReader) (*advertisement, error) {
	adv, err := readAdvertisement(p)
	if err != nil {
		return nil, err
	}
	return adv, f.req.check(adv)
}

// fetchV2 writes a protocol v2 fetch request to w and stores the pack of
// the response read from p.
func (f *fetcher) fetchV2(adv *advertisement, w io.Writer, p *pktReader) (Result, error) {
	req, err := f.req.encodeV2(adv)
	if err != nil {
		return Result{}, err
	}
	// The request is written whole before reading: upload-pack reads all of
	// a v2 request before answering.
	if _, err := w.Write(req); err != nil {
		return Result{}, fmt.Errorf("fetch: sending request: %w", err)
	}
	var pack string
	su, err := readV2Response(p, f.req.format, f.req.shallowLimit(), f.receive(&pack))
	if err != nil {
		return Result{}, err
	}
	return f.result(su, pack)
}

// http runs a fetch over smart HTTP: protocol v2 when the server speaks it,
// else v0 stateless RPC; or over dumb HTTP when the server is not smart.
func (f *fetcher) http(ctx context.Context, ep *endpoint) (Result, error) {
	s, err := newHTTPSession(ctx, f.opts.Config, f.env, ep)
	if err != nil {
		return Result{}, err
	}
	defer s.close()
	body, p, err := s.discover(ctx)
	if err != nil {
		return Result{}, err
	}
	if p == nil {
		return f.dumb(ctx, s, body)
	}
	adv, err := f.advertisement(p)
	body.Close()
	if err != nil {
		return Result{}, err
	}
	var pack string
	var su shallowUpdate
	if adv.v2 {
		req, err := f.req.encodeV2(adv)
		if err != nil {
			return Result{}, err
		}
		p, reply, err := s.post(ctx, req, true)
		if err != nil {
			return Result{}, err
		}
		defer reply.Close()
		su, err = readV2Response(p, f.req.format, f.req.shallowLimit(), f.receive(&pack))
		if err != nil {
			return Result{}, err
		}
		return f.result(su, pack)
	}
	f.advShallow = adv.shallow
	reqBody, mode, err := f.req.encodeV0(adv)
	if err != nil {
		return Result{}, err
	}
	p, reply, err := s.post(ctx, reqBody, false)
	if err != nil {
		return Result{}, err
	}
	defer reply.Close()
	if su, err = readV0Response(p, f.req.format, f.req.depth > 0, f.req.shallowLimit(), mode, f.receive(&pack)); err != nil {
		return Result{}, err
	}
	return f.result(su, pack)
}
