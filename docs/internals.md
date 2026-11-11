# Internals

## Admin protocol

Every CLI verb is one admin operation with the same name and argument shape
([`api/api.go`](../api/api.go)); programs embed ovnfs through the same
operations. `ovnfs --json start` makes sure the daemon runs (concurrent starts
share one) and prints its status, whose `socket` is the admin socket.

On it, frames are JSON objects behind a little-endian u32 length:

1. the client sends `{"hello": {"client": "…"}}`;
2. the daemon replies `{"welcome": {"version": 5, "ops": […], "features": […], …}}`,
   where `features` names optional request fields it honors (`api.Features`:
   `new.base`, `diff.binary`, `ls.select`, `stop.keep_mount`);
3. each request is `{"id": N, "<op>": {args}}`, answered by
   `{"id": N, "ok": reply}` or `{"id": N, "error": {"code", "message"}}`.

To cancel one outstanding request, send `{"cancel": N}` on the same
connection. This control frame has no acknowledgment of its own; the original
request returns error code `canceled` once its handler stops. Completion may
race with cancellation. Unknown or completed IDs are ignored, and request IDs
must be unique while active. Other requests and subscriptions remain live;
disconnecting cancels every active request on that connection.

`admin.Client.Call` sends cancellation when its context is canceled or its
deadline expires, returning that context's error. Cancellation is cooperative:
it does not roll back completed mutations or interrupt an I/O call, lock
acquisition or individual regexp operation already in progress. Search
workers finish cleanup before returning, closing files, unmapping buffers and
releasing the checkout's shared freeze lock.

`ovnfs ctl OP 'JSON'` sends one raw request; `--json` makes any verb print the
raw reply, `restart` the new daemon's status and `version` its build
(`{"version", "go", "os", "arch"}`); `run` keeps stdout for its command. Only
the socket owner's uid (on Windows, the same user SID) may
connect, on this socket and on the link socket.

`grep` and `glob` use the shapes in [`api/search.go`](../api/search.go).
For example, `ovnfs ctl grep '{"checkout":"app","pattern":"TODO","glob":"*.go"}'`
searches the live merged checkout without reading through the mount. A `dir`
under the configured mount is resolved to the same direct overlay access;
other absolute directories use native host I/O. Searches hold the checkout's
shared freeze lock until their workers finish, so snapshot/removal waits for
an active search; ordinary writes can continue. That wait never holds the
registry lock, so a slow file call or search stalls only its own checkout.

Searches through the daemon skip AppleDouble `._` sidecars, which the mount
does not list.

`profile` captures one Go runtime profile from the daemon (`cpu`, `heap`,
`allocs`, `goroutine`, `block`, `mutex` or `threadcreate`) and returns it as
base64 pprof bytes in its reply; `cpu`, `block` and `mutex` sample over a window
(`--seconds`, default 30), the rest are snapshots. `ovnfs profile cpu -o cpu.pb.gz`
writes the decoded bytes, which `go tool pprof cpu.pb.gz` reads.

Frames are limited to 16 MiB. An oversized reply returns error code
`too_large` without closing the connection; search clients can reduce their
result count, file filters or reported line width.

When omp finds the ovnfs binary, it tells its agents to open ovnfs checkouts
instead of git worktrees or copies, and uses this protocol to give
each isolated subagent its own checkout (`new` when it starts, `rm` when its
changes are merged back). The checkout's capture is the state the subagent
started from, so omp takes no baseline of it: the subagent's patch is
`diff {binary}`, and a branch commit reads its baseline from a second checkout
on the same capture (`new {base}`). A capture holding nested repositories or
submodules, or a daemon without those features, gets omp's own baseline
instead. Its `grep` and `glob` tools learn the daemon's mount
and admin socket from `ovnfs --json status` (nothing is read from the mount)
and send `grep`/`glob` to the daemon for search paths below that mount,
falling back to reading through the mount when it refuses.

## Persistence

State is `state.json` (version 5), atomically replaced with one durable write
per operation (`snap`, `fork` and `new --from` each sync once); multi-step
operations first write an intent that restart rolls forward or back. A
snapshot's tree id is written without a sync: a restart that lost it hashes the
snapshot again. Layer stacks are written only where they cannot be rebuilt from
a snapshot's parent. Builds that read only version 4 would cut store packs at
frame kinds they do not know (chunks, chunk manifests, tree deltas), so version
5 makes them refuse the state before they open the store; this build reads
version 4 and rewrites it as 5 when it opens. State of any other version is
refused. File calls never write state: a checkout's last write through its
folder (`ls` TOUCHED) is saved with the next operation or the planned stop, so
a crash forgets the writes since.

| path under state | holds |
| --- | --- |
| `bases/<id>/` | a capture (`capture.bin`: compressed binary per-file stat data; bases from older builds keep `capture.json`), its lock, index copies, the git objects its checkouts commit, and `git-template/`, the private git directory its prepared checkouts copy |
| `layers/` | frozen uppers until hashed, then only their non-source files and, above 1 KiB, the layer's compressed manifest; smaller manifests live in `state.json`, so such a stored-only layer needs no file or directory |
| `uppers/` | each checkout's writable copy-on-write layer, named after the checkout, or `_p<id>` for one prepared before it had a name |
| `git/` | private git directories, named like the uppers; a snapshot keeps its checkout's as a store tree, so an unchanged index costs nothing per snapshot |
| `store/` | git-format objects in append-only `packs/<n>.pack` (a full pack also gets `<n>.idx`); large blobs use content-defined chunks, or plain files under `objects/` when reflinks work |
| `sources/` | each source folder's stat cache: which files still match the blob ids of its last capture's tree |
| `nodes.log` | stable node ids (NFS handles, inode numbers); survive restarts and follow renames. A file's id is derived from its directory and name, so reading a tree logs only its directories; moved files and the rare name-hash collision are logged too. Ids stay below 2^53 so JavaScript runtimes see exact inode numbers |
| `mirror.git/` | shallow commits fetched from git upstreams of attached checkouts (see [remote](remote.md#committed-content-from-the-git-upstream)) |
| `node_id` | this daemon's random id; attach offers carry it so the remote tells this daemon's folders apart from other machines' |

Repeated ordinary `new` captures share an existing base when a fresh,
stat-cache-backed capture matches its tree, file/directory metadata, rules,
manifest lock and git HEAD/index/config/refs. The metadata (full mode, mtime,
size and inode of every walked entry) is hashed as the capture walks, so the
check costs no second pass over the folder. Private git directories remain
independent. Revision/take captures, nested repositories, linked worktrees,
busy repositories and captures whose identity cannot be proved are not reused.
Simultaneous first captures can still create distinct bases.

Hashing a snapshot reads and writes only the trees along its changed paths.
Changed trees and small files can be zstd deltas against their same-path parent;
renamed files use their recorded source before layer deletions are applied.
Deltas must halve storage, and chains contain at most 8 deltas. At the cap, the
next delta uses the chain's full root rather than forcing a full new object.
A snapshot's private git index uses the same blob storage.

Blobs over 1 MiB that cannot be reflinked, including streamed/byte input, are
content-defined chunks of 8/approximately 16/32 KiB minimum/average/maximum,
cut where a gear hash of the last 64 bytes has its top 13 bits clear. A chunk
already stored is only marked fresh, not compressed again.
Private chunk hashes deduplicate their bytes while a manifest retains the whole
Git blob id and logical size. Manifest pages hold up to 128 references and reuse
unchanged pages. Overwrites, insertions and appends therefore store changed
chunks rather than full copies; insertions can regroup subsequent metadata
pages. Random reads decode only the requested chunks. Reflinkable files stay
loose, and an already-chunked history stays chunked.

Once hashing pauses for 200 ms (or the daemon stops), each hashed layer writes
its manifest and drops the source files the store now holds; non-source files
remain raw, and the layer's own store tree keeps its stored blobs. A manifest of
up to 1 KiB (a snapshot of about 15 changed files) then moves into `state.json`,
which is synced before the file is removed: a file of its own would take a
whole filesystem block. Larger manifests stay files, since every registry change
rewrites `state.json` with everything embedded in it. Small manifest files left
by a crash or an older build move into `state.json` on reopen.
Copy-up and Git hashing still process whole changed files; chunking reduces
retained history, not the work of computing a Git SHA-1.

Unreferenced layers, bases and store objects are collected in the background.
Captures read the source once: files over 1 MiB are cloned where the filesystem
shares blocks (APFS `clonefile`, Linux `FICLONE` reflinks on btrfs, XFS and
bcachefs, ReFS and Dev Drive block cloning on Windows), otherwise staged and
streamed into chunks; smaller objects are compressed into the current pack.
Packed reads check CRC-32C and the
frame id; rebuilt deltas also verify the typed Git hash. Chunk reads verify
private content hashes, internal manifest pages verify theirs, and whole chunked
blob reads/materialization verify the Git hash. Plain loose reads do not rehash
content. There is no optional verify-on-read mode. A restart cuts a pack's torn
tail. Collection retains transitive delta bases and chunk dependencies,
following each dependency once per sweep. It waits for running captures and,
without holding up reads or other writes, for chunked ingests in flight, and
replaces `store/epoch` before it removes anything; a folder's stat cache trusts its
object ids without checking the store only while that epoch is unchanged. A
pack is rewritten, its live frames copied without recompressing, only once a
quarter of it is garbage.

On restart the server binds its persisted port, or the NFS socket a macOS mount
dials, and serves before touching the mount path, so hard-mounted NFS I/O
resumes with existing handles and cwd.
`restart` therefore leaves an NFS or SMB mount in place (`stop` with
`keep_mount`): the old daemon cuts its file connections before it closes the
export, and the client retries until the new one serves. From the cut on the
old daemon makes no call into the mount, such as the NFS root refresh after a
create or removal: with no server it would wait for the new daemon, which
waits for the old one to exit. A plain `stop`
unmounts before it replies; while the mount is busy it fails and the daemon
keeps running, and the next `stop` forces the unmount. The daemon closes its
admin socket before its final flush, hash drain and state write, which can take
a while under load; `stop` and `restart` wait for its process to exit, when the
state lock is released, and `restart` holds the start lock throughout, so
auto-starting clients wait for the new daemon rather than failing on the state
lock. A starting daemon opens its admin socket before it loads its state, which
takes minutes on a large state, and holds each hello until it has mounted: a
client that arrives during the load waits up to five minutes without loading progress (configurable with
`--daemon-timeout`) rather than starting a second daemon. The additive `probe`
handshake reports loading progress without exposing a partial registry; older
hello clients still wait for full readiness. A timeout reports the holder and
leaves it running. Increasing restored-record counts reset the idle budget,
including for clients waiting on another starter's lock. Shell completion gives up after a second.
Daemons starting together take turns replacing a stale socket, so only one
listens, and a client whose daemon fails its load quotes the daemon's log.

SIGTERM keeps NFS and SMB mounted for a replacement daemon, including when recovery escalates to KILL. Explicit `stop` and SIGINT unmount. On macOS, shutdown and recovery unmount helpers leave a durable receipt; every serving entry checks it before loading or mounting, so a blocked old helper cannot remove a replacement mount later.
Metadata uses `fsync(2)`, not `F_FULLFSYNC`: it survives server crashes, not
power loss. `state.json` is synced after its rename, so a kernel crash during
that sync can leave it empty on filesystems that do not order data before
renames (ext4's defaults do). Store objects are synced only when a full pack is
sealed or a pack is rewritten. `snap --wait` waits for hashing, not an object
durability barrier: a kernel crash or power loss can lose active-pack appends
even when snapshot metadata was synced.

## Fast creates

A folder create needs the folder as it is when the create arrives. The daemon
keeps the latest capture of each folder it created checkouts of and watches the
folder; a create first waits until every change made before it was reported
(`watch.Watcher.Sync`), then reuses that capture unless a change since it began
can alter it. Edits below cache, local or hidden roots, and git's object, log and
lock files, cannot. Creates that arrive while a capture runs share it: the
capture is a base no checkout is on yet, and once it is the folder's capture
every waiting create opens on it.

A capture of an unchanged file reads no content and no repository data: the
folder's stat cache (`sources/`) holds each file's blob id, including tracked
files a capture found to hold their index blob unconverted, whose blob the
repository keeps while its index still names it. Attribute and blob-size
checks run only for a tracked file the cache does not know. The walk
classifies a directory's entries, then lists the
source directories among them on eight readers while it descends; directories
it leaves alone are never listed, since listing one can update its access time,
which the watcher reports as an edit.

Checkouts on such a capture are prepared ahead of time, as many as the most
creates of the folder seen running at once (at least 8, at most 256), as far as
their private git directories fit 256 MiB (at least one): upper, private git
directory copied from the capture's `git-template/`, and overlay, below
directories named `_p<id>`. A create names one, which writes only its git directory's
`ovnfs.checkout` marker, and publishes it with a state write shared with
concurrent creates. When only work-tree files changed, the prepared checkouts
move onto the new capture, whose `git-template/` is written after the creates
waiting on it have their checkouts; a change below `.git` discards them.
Prepared checkouts are in no state: a restart removes them, and an unused
capture is released after 15 minutes.

## Memory tiers

Written files start in memory, not in the upper: new and truncated files, and
files of at most 1 MiB copied up for writing, share one budget across all
checkouts (`--memory`). A file moves into its upper once it has been left
untouched for 2 s, when it outgrows 1 MiB, when newer files need the budget
(least recently used first), before a hard link, `chown` or lock, and before
`snap`, `fork`, `diff` and a planned stop, which therefore cost one host create
per file written in the last 2 s; files deleted sooner never reach the disk.
Directories, symlinks and whiteouts always live in the upper. With
`--fsync` enabled, syncing a named memory file also spills and syncs it;
the default leaves it in memory. NFS does not forward application fsync calls.
This sync does not include the parent directory or snapshot data.

Small store blobs (at most 1 MiB) are served from a second budget
(`--blob-cache`) instead of their object files. `ovnfs status` shows both. A
crash or `kill -9` loses the files still in memory; `--memory 0` writes every
file to the upper as it happens.

Node ids are outside both budgets: the daemon keeps every path it has handed
out until it is removed, a file as a name in its directory's table rather than
a full node, about 150 bytes per path. A restart drops the unlogged ids, which
resolve again by listing their directory when a client presents them.
