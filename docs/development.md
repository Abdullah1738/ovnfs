# Development

```sh
go build -o bin/ovnfs .
go test ./...
```

`go test ./...` runs the package and in-process protocol checks; on Windows
with WinFsp installed it also mounts real WinFsp drives. The mesh relay
integration tests in `mesh/node` and `mesh/relay` build and run Stencil's relay
service from source: set `OVNFS_RELAY_SRC` to its directory, or they skip.
`OVNFS_RELAY_LIVE=1` also runs one against the production relay.

## Benchmark

`bench.sh REPO [TOUCH]` compares Cargo build times across ways of getting a
second working tree of REPO: a git worktree, an ovnfs checkout over each
protocol (NFS and SMB on macOS; FUSE, NFS and SMB on Linux, mounting the latter
two through passwordless sudo), and a copy-on-write clone. It times create,
snapshot, fork, cold, no-op and incremental builds; TOUCH (default
`src/main.rs`) is the file touched for incremental rows.

Results go to `BENCH_DIR/results.tsv`, by default `ovnfs-bench` beside REPO so
the clone and the daemon's state share its volume. `BUILD` (default
`cargo build`), `OVNFS` (default `bin/ovnfs`) and `PROTOS` override the rest;
run BUILD without cross-tree caches such as sccache, which would hide the
filesystem's effect. `bench.sh --clean REPO` removes `BENCH_DIR` and its
worktree.
