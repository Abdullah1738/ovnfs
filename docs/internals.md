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

State is `state.json` (version 4), atomically replaced with one durable write
per operation (`snap`, `fork` and `new --from` each sync once); multi-step
operations first write an intent that restart rolls forward or back. A
snapshot's tree id is written without a sync: a restart that lost it hashes the
snapshot again. Layer stacks are written only where they cannot be rebuilt from
a snapshot's parent. State of any other version is refused.

| path under state | holds |
| --- | --- |
| `bases/<id>/` | a capture (`capture.bin`: compressed binary per-file stat data; bases from older builds keep `capture.json`), its lock, index copies and the git objects its checkouts commit |
| `layers/` | immutable snapshot layers: the frozen upper until its snapshot is hashed, then a manifest (`.wh..wh.manifest`) with any non-source files, or just the manifest file, since its source files then live in the store |
| `uppers/` | each checkout's writable copy-on-write layer |
| `git/` | private git directories; a snapshot keeps its checkout's as a store tree, so an unchanged index costs nothing per snapshot |
| `store/` | git-format objects: up to 1 MiB zstd-compressed in append-only `packs/<n>.pack` (a full pack also gets `<n>.idx`), larger ones as plain files under `objects/` |
| `sources/` | each source folder's stat cache: which files still match the blob ids of its last capture's tree |
| `nodes.log` | stable node ids (NFS handles, inode numbers); survive restarts and follow renames. A file's id is derived from its directory and name, so reading a tree logs only its directories; moved files and the rare name-hash collision are logged too. Ids stay below 2^53 so JavaScript runtimes see exact inode numbers |
| `mirror.git/` | shallow commits fetched from git upstreams of attached checkouts (see [remote](remote.md#committed-content-from-the-git-upstream)) |

Hashing a snapshot reads and writes only the trees along its changed paths. A
changed file is stored as a zstd delta against the blob its path had in the
parent snapshot when that is at most half its own size (chains stay under 8
deltas), so a one-line edit to a large file costs about the edit; a snapshot of
a git checkout stores its rewritten index the same way. Once hashing pauses for
200 ms (or the daemon stops), each hashed layer writes its manifest and drops
the source files the store now holds; its own store tree keeps those blobs.

Unreferenced layers, bases and store objects are collected in the background.
Captures store each new object with one read: files over 1 MiB are cloned where
the filesystem has reflinks or `clonefile`, others compressed into the current
pack. Every packed read checks the frame's checksum and id, and a restart cuts a
pack's torn tail. Collection waits for running captures and replaces
`store/epoch` before it removes anything; a folder's stat cache trusts its
object ids without checking the store only while that epoch is unchanged. A
pack is rewritten, its live frames copied without recompressing, only once a
quarter of it is garbage.

On restart the server binds its persisted port and serves before touching the
mount path, so hard-mounted NFS I/O resumes with existing handles and cwd.
Metadata uses `fsync(2)`, not `F_FULLFSYNC`: it survives server crashes, not
power loss. `state.json` is synced after its rename, so a kernel crash during
that sync can leave it empty on filesystems that do not order data before
renames (ext4's defaults do). Store objects are synced only when a full pack is
sealed or a pack is rewritten.

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
