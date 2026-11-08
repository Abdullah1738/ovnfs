# Remote checkouts

## Attach

`ovnfs attach CHECKOUT|DIR --to HOST` snapshots the checkout (a folder is
captured first) and opens a link to the other machine: `ssh HOST ovnfs link`,
which pipes the session into HOST's daemon, starting it if needed. `--remote-bin`
names ovnfs on hosts where it is not on the PATH. ssh runs in batch mode, so
HOST must accept a key without prompting; whatever reaches HOST by ssh works,
including `ProxyJump` and other `~/.ssh/config` settings.

The remote rebuilds the snapshot's tree from the offered entries, checks it
hashes to the offered id, and shows the checkout at once: listing and stat
never move content. The first open of a file fetches its blob over the link,
verifies it against its id, and keeps it in the remote's content-addressed
store, shared by every checkout there; that open also prefetches the small
uncached files of its directory subtree in one batch. Cache roots arrive as
empty directories; local and hidden paths never leave. Writes on the remote go
to its own copy-on-write upper, and `diff`, `snap` and `fork` work there.

### Committed content from the git upstream

When a repository in the snapshot has an upstream, the offer also names its
URL (without credentials) and a commit there: HEAD when the upstream contains
it, else HEAD's merge base with the upstream. The remote fetches that commit,
without history, into `mirror.git` in its state directory, in the background
and with this machine's git credentials (credential helpers, askpass programs,
`http.extraHeader`, ssh keys), through `git fetch` when git is installed and
ovnfs's own fetch otherwise. It never prompts, ssh runs in batch mode unless
`GIT_SSH_COMMAND` or `GIT_SSH` says otherwise, and only https, http, ssh, git
and local transports are used. Until the commit arrives,
reads go over the link as above. After that, every file whose content the
commit holds is read from the mirror, verified against its id like any fetched
blob, and only edited and untracked files cross the link; opening a file the
mirror holds prefetches nothing. A remote that cannot fetch (no network, no
credentials, an unpushed commit) reports a fault and keeps reading over the
link. Commits stay in the mirror across restarts and for later attaches; delete
`mirror.git` to reclaim its space.

The link reconnects with backoff while the source daemon runs. Without it,
cached content stays readable and uncached reads fail with EIO; [push](#push)
first to cache all of it. A slow download
for an open does not hold the overlay's namespace lock: unrelated reads and
writes can proceed. Re-attaching the same snapshot reuses the remote checkout.
A newer snapshot updates the same
lane in place when its changes are only cache/local-class paths: its name,
path, open handles and warm build outputs stay put. Source changes keep the old
lane and open the new snapshot beside it.

### Search

`grep` and `glob` in an attached checkout walk its metadata on the remote, so
listing costs no round trips. Before walking, they fetch the ignore files the
walk reads in one batch; reading a cached file never prefetches its siblings.
Grep then scans the files the remote has neither fetched nor mirrored where
their bytes are: the remote sends batches of blob ids with the pattern over the
link, the source machine scans them, and only the results come back, so nothing
they read enters the remote's store. Files written on the remote, fetched files
and files the git mirror holds are scanned locally, and the results merge in the
usual path order. If the link fails a batch, grep reads those files through the
checkout instead, fetching them as any read does. Searching repeatedly costs a
round trip each time, since scanned files are never fetched.

## Send

`ovnfs send [DIR|CHECKOUT] HOST[:PATH]` attaches to a machine you reach by ssh,
preparing it first, so one command takes a folder to a fresh box:

```sh
ovnfs send . devbox:src/app    # devbox:~/src/app now shows this folder
```

1. Without ovnfs on HOST (`--remote-bin`, else on its PATH, else
   `~/.local/bin/ovnfs`), send downloads the release binary for HOST's
   `uname` (Linux or macOS, amd64 or arm64) here from `build.stencil.so` and
   installs it as `~/.local/bin/ovnfs` over ssh, so HOST needs no network
   access of its own.
   The release is this binary's own, so both ends speak the same protocols,
   or the latest one when this is a development build.
2. It attaches over ssh with that binary, which starts HOST's daemon. The
   remote name defaults to the folder's name, as for `run --on`, so repeated
   sends keep one lane name.
3. With `:PATH`, it points a symlink at PATH on HOST (relative to its home;
   `~/` expands there) to the attached checkout, replacing one from an earlier
   send. Anything else at PATH stops send before it attaches.

The scripts send runs on HOST need a POSIX `sh`.

## Pull

`ovnfs pull CHECKOUT [--from HOST] [--dry-run]` brings the attached remote
checkout's source edits into the local checkout, not the original folder. Paths
left alone locally take the remote version and text changed on both sides is
merged as [apply](checkouts.md#diff-and-apply) does, with conflict markers
labelled `local` and `remote NAME` to resolve in the local checkout before
pulling again; other conflicts are reported without touching those paths.
Pulling again makes no further changes; a dry run reports without writing.
`--from` is needed only when the checkout is attached to more than one machine,
and the link must be up.

Then `ovnfs apply CHECKOUT` merges the local checkout back into the folder.
Attaching a folder prints the local checkout name to use for pull and apply.

## Push

`ovnfs push [CHECKOUT] [--to HOST]` makes attached machines independent of this
one: each fetches, over its link, every blob it would still read from here, so
its checkouts keep working after this machine sleeps, leaves the network or
stops its daemon.

```sh
ovnfs attach app --to devbox
ovnfs push app                 # devbox now holds all of app's source content
```

Without arguments push covers every attachment; `CHECKOUT` and `--to` narrow
it. On the remote it covers every checkout and snapshot attached from the same
source folder, whichever lane holds it, including symlinks and files only the
local checkout had; files its [git mirror](#committed-content-from-the-git-upstream)
holds, cache roots, and local or hidden paths stay out. The remote fetches with
ordinary verified requests, in batches of at most 8 MiB or 1024 blobs, so its
reads keep being answered during a long push. On a terminal, push draws one
progress line on stderr summing its attachments; `subscribe` streams the same
reports as `pushing` events (`checkout`, `to`, `blobs`, `bytes`, `total_blobs`,
`total_bytes`), the totals first and then at most four a second per
attachment. Each attachment reports the blobs and bytes it fetched; one that
is disconnected or fails is reported, with how far it got, without stopping the
others, and push then exits 1. Blobs fetched before a failure or
an interrupt stay, so pushing again resumes. Push changes nothing the remote's
checkouts show; edits made here after the attach still need another `attach`.

## Share

`ovnfs share CHECKOUT [--rw]` serves a checkout over remote-fs on a Unix
socket, the protocol a vibevmm VM's `home` device speaks
(`vmon --remote-fs home:SOCK[:rw]`).
