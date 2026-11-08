# Internals

## Admin protocol

Every CLI verb is one admin operation with the same name and argument shape
([`api/api.go`](../api/api.go)); programs embed ovnfs through the same
operations. `ovnfs --json start` makes sure the daemon runs (concurrent starts
share one) and prints its status, whose `socket` is the admin socket.

On it, frames are JSON objects behind a little-endian u32 length:

1. the client sends `{"hello": {"client": "…"}}`;
2. the daemon replies `{"welcome": {"version": 5, "ops": […], …}}`;
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
raw reply. Only the socket owner's uid (on Windows, the same user SID) may
connect, on this socket and on the link socket.

`grep` and `glob` use the shapes in [`api/search.go`](../api/search.go).
For example, `ovnfs ctl grep '{"checkout":"app","pattern":"TODO","glob":"*.go"}'`
searches the live merged checkout without reading through the mount. A `dir`
under the configured mount is resolved to the same direct overlay access;
other absolute directories use native host I/O. Searches hold the checkout's
shared freeze lock until their workers finish, so snapshot/removal waits for
an active search; ordinary writes can continue.

Searches through the daemon skip AppleDouble `._` sidecars, which the mount
does not list.

Frames are limited to 16 MiB. An oversized reply returns error code
`too_large` without closing the connection; search clients can reduce their
result count, file filters or reported line width.

When omp finds the ovnfs binary, it tells its agents to open ovnfs checkouts
instead of git worktrees or copies, and uses this protocol to give
each isolated subagent its own checkout (`new` when it starts, `rm` when its
changes are merged back). Its `grep` and `glob` tools learn the daemon's mount
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
refused.

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
has reflinks or `clonefile`, otherwise staged and streamed into chunks; smaller
objects are compressed into the current pack. Packed reads check CRC-32C and the
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

On restart the server binds its persisted port and serves before touching the
mount path, so hard-mounted NFS I/O resumes with existing handles and cwd.
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

Checkouts on such a capture are prepared ahead of time, as many as the largest
burst of creates seen (at least 8, at most 256): upper, private git directory
copied from the capture's `git-template/`, and overlay, below directories named
`_p<id>`. A create names one, which writes only its git directory's
`ovnfs.checkout` marker, and publishes it with a state write shared with
concurrent creates. When only work-tree files changed, the prepared checkouts
move onto the new capture; a change below `.git` discards them.
Prepared checkouts are in no state: a restart removes them, and an unused
capture is released after 15 minutes.

## Memory tiers

Written files start in memory, not in the upper: new and truncated files, and
files of at most 1 MiB copied up for writing, share one budget across all
checkouts (`-memory`). A file moves into its upper once it has been left
untouched for 2 s, when it outgrows 1 MiB, when newer files need the budget
(least recently used first), before a hard link, `chown` or lock, and before
`snap`, `fork`, `diff` and a planned stop, which therefore cost one host create
per file written in the last 2 s; files deleted sooner never reach the disk.
Directories, symlinks and whiteouts always live in the upper.

Small store blobs (at most 1 MiB) are served from a second budget
(`-blob-cache`) instead of their object files. `ovnfs status` shows both. A
crash or `kill -9` loses the files still in memory; `-memory 0` writes every
file to the upper as it happens.

Node ids are outside both budgets: the daemon keeps every path it has handed
out until it is removed, a file as a name in its directory's table rather than
a full node, about 150 bytes per path. A restart drops the unlogged ids, which
resolve again by listing their directory when a client presents them.
