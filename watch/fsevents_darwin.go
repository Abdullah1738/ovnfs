//go:build darwin && cgo

package watch

/*
#cgo LDFLAGS: -framework CoreServices -framework CoreFoundation
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdint.h>
#include <stdlib.h>

extern void watchCallback(uintptr_t ctx, size_t n, char **paths, uint32_t *flags, uint64_t *ids);

static void watch_cb(ConstFSEventStreamRef s, void *info, size_t n, void *paths,
		const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
	watchCallback((uintptr_t)info, n, (char **)paths, (uint32_t *)flags, (uint64_t *)ids);
}

static dispatch_queue_t watch_queue(void) {
	return dispatch_queue_create("ovnfs.watch", DISPATCH_QUEUE_SERIAL);
}

static void watch_queue_release(dispatch_queue_t q) {
	dispatch_release(q);
}

static uint64_t watch_now(void) {
	return FSEventsGetCurrentEventId();
}

// watch_start creates a file-level stream over paths delivering events after
// since on q, or returns NULL.
static FSEventStreamRef watch_start(uintptr_t ctx, char **paths, int n, double latency, dispatch_queue_t q, uint64_t since) {
	CFMutableArrayRef arr = CFArrayCreateMutable(NULL, n, &kCFTypeArrayCallBacks);
	for (int i = 0; i < n; i++) {
		CFStringRef p = CFStringCreateWithFileSystemRepresentation(NULL, paths[i]);
		if (p == NULL) {
			CFRelease(arr);
			return NULL;
		}
		CFArrayAppendValue(arr, p);
		CFRelease(p);
	}
	FSEventStreamContext c = {0, (void *)ctx, NULL, NULL, NULL};
	FSEventStreamRef s = FSEventStreamCreate(NULL, watch_cb, &c, arr, since, latency,
		kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagWatchRoot);
	CFRelease(arr);
	if (s == NULL) {
		return NULL;
	}
	FSEventStreamSetDispatchQueue(s, q);
	if (!FSEventStreamStart(s)) {
		FSEventStreamInvalidate(s);
		FSEventStreamRelease(s);
		return NULL;
	}
	return s;
}

static void watch_noop(void *p) {}

// watch_stop stops s and returns once no callback for it is running or
// queued on q.
static void watch_stop(FSEventStreamRef s, dispatch_queue_t q) {
	FSEventStreamStop(s);
	FSEventStreamInvalidate(s);
	dispatch_sync_f(q, NULL, watch_noop);
	FSEventStreamRelease(s);
}
*/
import "C"

import (
	"errors"
	"os"
	"unsafe"
)

// latency is how long FSEvents gathers events after the first of a burst
// (delivered at once thanks to NoDefer) before delivering the rest.
const latency = 0.05

// Event flags that leave the changed paths unknown.
const (
	flagScan    = C.kFSEventStreamEventFlagMustScanSubDirs | C.kFSEventStreamEventFlagRootChanged
	flagDropped = C.kFSEventStreamEventFlagUserDropped | C.kFSEventStreamEventFlagKernelDropped
	// flagNoPath marks bookkeeping events whose path names no change.
	flagNoPath = C.kFSEventStreamEventFlagHistoryDone | C.kFSEventStreamEventFlagEventIdsWrapped
)

// queue is a serial dispatch queue that runs FSEvents callbacks.
type queue struct{ q C.dispatch_queue_t }

func newQueue() queue { return queue{C.watch_queue()} }

func (q queue) release() { C.watch_queue_release(q.q) }

// now returns the most recent system-wide FSEvents event id.
func now() uint64 { return uint64(C.watch_now()) }

// canonical returns path as FSEvents reports it: Darwin's realpath resolves
// symlinks (/tmp -> /private/tmp) and, unlike filepath.EvalSymlinks, also the
// on-disk case of every component on case-insensitive volumes.
func canonical(path string) (string, error) {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	r, err := C.realpath(p, nil)
	if r == nil {
		return "", &os.PathError{Op: "realpath", Path: path, Err: err}
	}
	defer C.free(unsafe.Pointer(r))
	return C.GoString(r), nil
}

// stream is a running FSEvents stream.
type stream struct {
	ref   C.FSEventStreamRef
	q     queue
	paths []string // the canonical directories it watches recursively
}

// startStream watches paths (canonical) recursively, starting with events
// after id since (replayed from FSEvents' history); callbacks run on q and
// call watchCallback with ctx.
func startStream(ctx uintptr, paths []string, q queue, since uint64) (*stream, error) {
	cpaths := make([]*C.char, len(paths))
	for i, p := range paths {
		cpaths[i] = C.CString(p)
	}
	defer func() {
		for _, p := range cpaths {
			C.free(unsafe.Pointer(p))
		}
	}()
	// cpaths holds only C pointers, so C may read it during the call.
	ref := C.watch_start(C.uintptr_t(ctx), &cpaths[0], C.int(len(paths)), C.double(latency), q.q, C.uint64_t(since))
	if ref == nil {
		return nil, errors.New("watch: FSEventStreamStart failed")
	}
	return &stream{ref: ref, q: q, paths: paths}, nil
}

// stop stops s; no callback for s runs once it returns.
func (s *stream) stop() { C.watch_stop(s.ref, s.q.q) }
