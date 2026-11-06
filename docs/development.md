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

## CI and releases

[`ci.yml`](../.github/workflows/ci.yml) runs on pushes to `main` and on pull
requests: `gofmt`, `go mod tidy -diff`, `go vet` for Linux, macOS and Windows,
a snapshot GoReleaser build, then `go test` on all three (race detector on
Linux and macOS, real FUSE mounts on Linux, real WinFsp drives on Windows).

Pushing a `v*` tag runs the same checks, then
[`release.yml`](../.github/workflows/release.yml) has GoReleaser
([`.goreleaser.yaml`](../.goreleaser.yaml)) publish a GitHub release with
archives for Linux, macOS and Windows on amd64 and arm64:

```sh
git tag v0.2.0 && git push origin v0.2.0
```

Go stamps the tag into the build info, so `ovnfs version` reports it for release
archives and `go install …@v0.2.0` alike, and a VCS pseudo-version in a clone.

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
