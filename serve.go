package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	ovnfuse "github.com/stencil-hq/ovnfs/fuse"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/smb"
	"github.com/stencil-hq/ovnfs/winfsp"
)

// memoryFileMax is the largest written file kept in memory; bigger files go
// to disk, where writes already land in the page cache.
const memoryFileMax = 1 << 20

// memoryIdle is how long a file sits untouched in memory before it is written
// back to its upper, bounding what a snapshot must flush and a crash loses.
const memoryIdle = 2 * time.Second

// byteSize is a flag holding a byte count: a number with an optional binary
// K, M, G or T suffix ("512M", "1G", "0").
type byteSize int64

// String renders the byte count in the largest suffix that divides it ("1G").
func (b *byteSize) String() string {
	n := int64(*b)
	for _, u := range []struct {
		shift  uint
		suffix string
	}{{40, "T"}, {30, "G"}, {20, "M"}, {10, "K"}} {
		if n != 0 && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// Set parses a size such as "512M"; "B", "KB" and "KiB" spellings are accepted.
func (b *byteSize) Set(v string) error {
	s := strings.TrimSuffix(strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(v)), "IB"), "B")
	shift := 0
	if i := len(s) - 1; i >= 0 {
		switch s[i] {
		case 'K':
			shift = 10
		case 'M':
			shift = 20
		case 'G':
			shift = 30
		case 'T':
			shift = 40
		}
		if shift != 0 {
			s = s[:i]
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64>>shift {
		return fmt.Errorf("invalid size %q", v)
	}
	*b = byteSize(n << shift)
	return nil
}

// sizeFlag registers a byteSize flag whose default an environment variable may override.
func sizeFlag(flags *flag.FlagSet, name, env string, def int64, usage string) (*byteSize, error) {
	b := byteSize(def)
	if v := os.Getenv(env); v != "" {
		if err := b.Set(v); err != nil {
			return nil, fmt.Errorf("%s: %w", env, err)
		}
	}
	flags.Var(&b, name, usage+", env "+env)
	return &b, nil
}

// eventHub fans persisted events out to connected admin subscribers.
type eventHub struct {
	mu          sync.Mutex
	subscribers map[chan admin.Event]context.CancelFunc
}

// emit delivers events without holding up registry operations for slow clients.
func (h *eventHub) emit(e checkout.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, cancel := range h.subscribers {
		select {
		case ch <- admin.Event{Name: e.Name, Payload: e.Payload}:
		default:
			cancel()
			delete(h.subscribers, ch)
			close(ch)
		}
	}
}

// events registers a bounded subscription whose cancellation releases its resources.
func (h *eventHub) events(ctx context.Context) <-chan admin.Event {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan admin.Event, 256)
	h.mu.Lock()
	if h.subscribers == nil {
		h.subscribers = make(map[chan admin.Event]context.CancelFunc)
	}
	h.subscribers[ch] = cancel
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			close(ch)
		}
		h.mu.Unlock()
		cancel()
	}()
	return ch
}

// serve opens the persistent export, attaches its native client, and handles shutdown.
func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	state := flags.String("state", defaultState, "persistent state directory")
	mount := flags.String("mount", defaultMount, "folder where checkouts appear")
	proto := flags.String("proto", defaultProto(), "nfs, smb, fuse (Linux) or winfsp (Windows)")
	address := flags.String("listen", net.JoinHostPort("127.0.0.1", strconv.Itoa(smbClientPort())), "file server address (nfs, smb)")
	memory, err := sizeFlag(flags, "memory", "OVNFS_MEMORY", 1<<30, "bytes of small written files kept in memory until they spill to disk; 0 writes every file to disk, surviving a crash")
	if err != nil {
		return err
	}
	blobCache, err := sizeFlag(flags, "blob-cache", "OVNFS_BLOB_CACHE", 512<<20, "bytes of small content-addressed blobs served from memory; 0 disables")
	if err != nil {
		return err
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("serve flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve flags: takes no positional arguments")
	}
	if *proto != "nfs" && *proto != "smb" && *proto != "fuse" && *proto != "winfsp" {
		return fmt.Errorf("serve flags: unknown protocol %q", *proto)
	}
	var mem *overlay.Memory
	if *memory > 0 {
		mem = overlay.NewMemory(int64(*memory), memoryFileMax, memoryIdle)
	}
	*state, err = filepath.Abs(*state)
	if err != nil {
		return fmt.Errorf("state path: %w", err)
	}
	*mount, err = filepath.Abs(*mount)
	if err != nil {
		return fmt.Errorf("mount path: %w", err)
	}
	if err = os.MkdirAll(*state, 0700); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	hub := &eventHub{}
	var e *export.Export
	// Folder events may invalidate before the export exists; they are dropped then.
	var exported atomic.Pointer[export.Export]
	var smbServer *smb.Server
	var checkoutShares *shares
	userHome, _ := os.UserHomeDir()
	lk := newLinks(hub.emit)
	md, err := openMesh(*state, lk)
	if err != nil {
		return fmt.Errorf("mesh: %w", err)
	}
	reg, err := checkout.Open(checkout.Config{State: *state, Mount: *mount, UserHome: userHome, Node: md.key.ID().String(), Emit: hub.emit, Fetch: lk.fetch, Search: lk.search,
		Memory: mem, BlobCache: int64(*blobCache),
		Retire: func(name string) {
			if checkoutShares != nil {
				checkoutShares.Close(name)
			}
			if e != nil {
				e.Retire(name)
			}
		},
		Invalidate: func(name string, paths []string) {
			if x := exported.Load(); x != nil {
				x.Invalidate(name, paths)
			}
		},
		Changed: func(name string, added bool) {
			if smbServer != nil {
				if added {
					smbServer.Changed(name, smb.NotifyAdded)
				} else {
					smbServer.Retire(name)
				}
			}
			if *proto == "nfs" {
				refreshRoot(*mount)
			}
		}})
	if err != nil {
		return fmt.Errorf("checkouts: %w", err)
	}
	defer reg.Close()
	lk.reg = reg
	lk.mesh = md
	defer lk.close()
	e, err = export.New(export.NewRouter(reg), filepath.Join(*state, "nodes.log"), *state)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	exported.Store(e)
	defer e.Close()
	if checkoutShares, err = newShares(*state, reg, e); err != nil {
		return fmt.Errorf("shares: %w", err)
	}
	defer checkoutShares.CloseAll()
	served := make(chan error, 2)
	port := 0
	release := func(force bool) error { return unmount(*mount, force) }
	if *proto == "fuse" || *proto == "winfsp" {
		srv, mountErr := mountInProcess(*proto, e, *mount)
		if mountErr != nil {
			return mountErr
		}
		release = func(bool) error { return srv.Unmount() }
		go func() { srv.Wait(); served <- fmt.Errorf("%s mount at %s ended", *proto, *mount) }()
		if err = reg.SetListen(*proto, 0); err != nil {
			_ = release(true)
			return fmt.Errorf("persist listen: %w", err)
		}
	} else if port, err = listenAndMount(reg, e, *proto, *address, *state, *mount, served, func(s *smb.Server) { smbServer = s }); err != nil {
		return err
	}
	log.Printf("mounted at %s", *mount)
	adminListener, err := admin.Listen(filepath.Join(*state, "admin.sock"))
	if err != nil {
		_ = release(true)
		return fmt.Errorf("admin listen: %w", err)
	}
	defer adminListener.Close()
	linkListener, err := linkListen(*state)
	if err != nil {
		_ = release(true)
		return fmt.Errorf("link listen: %w", err)
	}
	defer linkListener.Close()
	go lk.listen(linkListener)
	md.resume()
	defer md.stop()
	stopRequests := make(chan struct{}, 1)
	welcome := admin.Welcome{Version: protocolVersion, Ops: api.Ops, Features: api.Features, Proto: *proto, Port: port, Mount: *mount, State: *state}
	handler := &opsHandler{reg: reg, shares: checkoutShares, links: lk, mesh: md, welcome: welcome, started: time.Now(), stop: func() {
		select {
		case stopRequests <- struct{}{}:
		default:
		}
	}}
	go func() {
		served <- fmt.Errorf("admin serve: %w", admin.Serve(adminListener, welcome, handler, hub.events))
	}()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	force := false
	for {
		select {
		case err = <-served:
			_ = release(true)
			return err
		case <-stopRequests:
		case <-sigs:
		}
		if err = release(force); err != nil {
			log.Printf("unmount: %v; stop again to force", err)
			force = true
			continue
		}
		return nil
	}
}

// defaultProto picks the frontend an unprivileged user can mount on this OS.
// On Windows SMB, whose client caches under leases, is faster than WinFsp,
// which sends every open to the daemon; WinFsp wins only where installed and
// the SMB client dials port 445 alone, which the Server service holds (smbClientPort).
func defaultProto() string {
	switch runtime.GOOS {
	case "linux":
		return "fuse"
	case "windows":
		if smbClientPort() == 445 && winfsp.Installed() {
			return "winfsp"
		}
		return "smb"
	}
	return "nfs"
}

// inProcessMount is a mount whose filesystem driver calls back into this
// process (FUSE, WinFsp), so it ends with the daemon.
type inProcessMount interface {
	Unmount() error
	Wait()
}

// mountInProcess mounts e at mount through FUSE or WinFsp.
func mountInProcess(proto string, e *export.Export, mount string) (inProcessMount, error) {
	if proto == "winfsp" {
		if err := makeMountPoint(mount); err != nil {
			return nil, fmt.Errorf("mount directory: %w", err)
		}
		srv, err := winfsp.Mount(e, mount)
		if err != nil {
			return nil, fmt.Errorf("mount: %w", err)
		}
		return srv, nil
	}
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return nil, fmt.Errorf("mount directory: %w", err)
	}
	srv, err := ovnfuse.Mount(e, mount, ovnfuse.Options{})
	if err != nil {
		return nil, fmt.Errorf("mount: %w", err)
	}
	return srv, nil
}

// listenAndMount serves NFS or SMB on the persisted port and mounts it at mount
// with the system client, unless a previous run's mount is still there. An SMB
// client that dials one port only pins it, overriding an earlier run's port.
// SMB authenticates with the state directory's account where the OS has one
// (smbAccount), otherwise as guest.
func listenAndMount(reg *checkout.Registry, e *export.Export, proto, address, state, mount string, served chan<- error, smbStarted func(*smb.Server)) (int, error) {
	savedProto, savedPort := reg.Listen()
	if savedPort != 0 && savedProto != proto {
		return 0, fmt.Errorf("listen: state uses %s; restart with -proto %s", savedProto, savedProto)
	}
	pinned := 0
	var user, password string
	if proto == "smb" {
		pinned = smbClientPort()
		var err error
		if user, password, err = smbAccount(state); err != nil {
			return 0, err
		}
	}
	if want := cmp.Or(pinned, savedPort); want != 0 {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return 0, fmt.Errorf("listen address: %w", splitErr)
		}
		address = net.JoinHostPort(host, strconv.Itoa(want))
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		if pinned != 0 {
			return 0, fmt.Errorf("listen %s: %w; this Windows SMB client dials only port %d (Windows 11 24H2 and later dial any port), "+
				"which the Server service (LanmanServer) normally holds: install WinFsp (https://winfsp.dev), which ovnfs then mounts instead, "+
				"or from an elevated prompt run \"sc config LanmanServer start= disabled\" and \"net stop LanmanServer /y\" (this ends local file and printer sharing), then start ovnfs again",
				address, err, pinned)
		}
		if savedPort != 0 {
			return 0, fmt.Errorf("listen: cannot bind persisted %s port %d at %s (existing mounts require this port): %w", proto, savedPort, address, err)
		}
		return 0, fmt.Errorf("listen %s: %w", address, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = reg.SetListen(proto, port); err != nil {
		listener.Close()
		return 0, fmt.Errorf("persist listen: %w", err)
	}
	if proto == "smb" {
		s := &smb.Server{FS: e.SMB(), Share: "ovnfs", MountRoots: mountRoots(mount)}
		if user != "" {
			s.Users = map[string]string{user: password}
		}
		nodes := e.Nodes()
		e.OnRecall(func(id uint64) {
			if p, ok := nodes.Path(id); ok {
				go s.Changed(p, smb.NotifyModified)
			}
		})
		if os.Getenv("LOG_LEVEL") != "" {
			s.Logf = log.Printf
		}
		smbStarted(s)
		go func() { served <- fmt.Errorf("smb serve: %w", s.Serve(listener)) }()
	} else {
		go func() { served <- fmt.Errorf("nfs serve: %w", serveNFS(listener, e)) }()
	}
	if !mounted(mount) {
		if err = makeMountPoint(mount); err != nil {
			return 0, fmt.Errorf("mount directory: %w", err)
		}
		if err = mountServer(proto, port, mount, user, password); err != nil {
			return 0, fmt.Errorf("mount: %w", err)
		}
	}
	return port, nil
}
