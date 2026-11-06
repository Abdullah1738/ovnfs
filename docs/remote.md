# Remote checkouts

## Attach

`ovnfs attach CHECKOUT|DIR --to MEMBER|HOST` snapshots the checkout (a folder
is captured first) and opens a link to the other machine: a QUIC stream over
the [mesh](#mesh) when `--to` names a member, otherwise `ssh HOST ovnfs link`,
which relays the session into HOST's daemon, starting it if needed. Over the
mesh the offer must come from the node the connection proved. `--remote-bin`
names ovnfs on hosts where it is not on the PATH.

The remote rebuilds the snapshot's tree from the offered entries, checks it
hashes to the offered id, and shows the checkout at once: listing and stat
never move content. The first open of a file fetches its blob over the link,
verifies it against its id, and keeps it in the remote's content-addressed
store, shared by every checkout there; that open also prefetches the small
uncached files of its directory subtree in one batch. Cache roots arrive as
empty directories; local and hidden paths never leave. Writes on the remote go
to its own copy-on-write upper, and `diff`, `snap` and `fork` work there.

The link reconnects with backoff while the source daemon runs. Without it,
cached content stays readable and uncached reads fail with EIO. A slow download
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
Grep then scans the files the remote has not fetched where their bytes are:
the remote sends batches of blob ids with the pattern over the link, the source
machine scans them, and only the results come back, so nothing they read enters
the remote's store. Files written on the remote and fetched files are scanned
locally, and the results merge in the usual path order. If the link fails a
batch, grep reads those files through the checkout instead, fetching them as
any read does. Searching repeatedly costs a round trip each time, since
scanned files are never fetched.

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

## Share

`ovnfs share CHECKOUT [--rw]` serves a checkout over remote-fs on a Unix
socket, the protocol a vibevmm VM's `home` device speaks
(`vmon --remote-fs home:SOCK[:rw]`).

## Mesh

Machines that reach each other by key:

```sh
ovnfs mesh setup               # on the first machine: prints o2://join?mesh=…&node=…&s=…
ovnfs mesh join 'o2://…'       # on another: joins; both sides pin each other's key
ovnfs mesh ls                  # members, their node ids and ways
ovnfs attach app --to devbox   # now over the mesh
```

A node is its Ed25519 key (`<state>/mesh/node_key`, PKCS#8, and a self-signed
`node_cert.der`), the same identity and certificate format as Stencil's Rust
`stencil-net`, which `mesh/` ports to Go on quic-go. Connections are QUIC with
mutual TLS 1.3, each side pinning the other's key: a member is reached over a
direct UDP way (port 8377, Tailscale and private addresses first, raced 250 ms
apart) or, when none answers, through the Stencil relay (`my.omp.sh/node`,
`stencil-relay.v2`), which sees only QUIC packets. One connection carries one
protocol: `o2-mesh/1` for membership and `ovnfs-link/1` for attach links.

`mesh setup` starts a mesh on its first run (`--relay URL` picks another relay,
`--relay off` none) and mints a ticket each time: 32 random bytes, single use,
valid ten minutes, kept only as a hash. `mesh join` dials the token's node with
its key pinned, redeems the ticket and receives the roster. The roster lives in
every member's `<state>/mesh/roster.json`; each entry is a last-writer-wins
register per node, removals are tombstones, and members push it to each other
when it changes. Only members get past admission, except to redeem a ticket.
`mesh rm MEMBER` removes another machine (which leaves once told), `mesh leave`
this one.
