# ovnfs

[![ci](https://github.com/stencil-hq/ovnfs/actions/workflows/ci.yml/badge.svg)](https://github.com/stencil-hq/ovnfs/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/stencil-hq/ovnfs.svg)](https://pkg.go.dev/github.com/stencil-hq/ovnfs)

Checkouts for agents: isolated, copy-on-write views of any folder, a git
repository or not, opened without copying it. Agents work in
the checkout; you review with `diff` and merge back with a three-way `apply`.
Checkouts can be snapshotted, forked, and attached to another machine, where
their content arrives on first read.

One daemon serves every checkout as a folder of a single mount: NFSv3 on
macOS, FUSE on Linux, SMB or WinFsp on Windows. No root, and no kernel
extension beyond WinFsp where you install it.

## Install

```sh
go install github.com/stencil-hq/ovnfs@latest
```

needs Go (see `go.mod`). [Releases](https://github.com/stencil-hq/ovnfs/releases)
have prebuilt binaries for Linux, macOS and Windows on amd64 and arm64, and
`go build -o bin/ovnfs .` builds a clone. ovnfs runs git, and on Linux needs
`fusermount3` (the `fuse3` package). `ovnfs version` prints the build.

## Quick start

```sh
cd ~/src/app
cd "$(ovnfs new)"              # starts the daemon, prints the checkout's path
git status                     # same status as ~/src/app; nothing is written there
# ...let the agent work...
ovnfs diff app -p              # what changed
ovnfs apply app                # merge it back into ~/src/app
cd ~/src/app && ovnfs rm app   # remove the checkout; its snapshots stay
```

More, from `~/src/app`:

```sh
ovnfs run -- make test         # a disposable checkout: run, then remove
ovnfs new --rev main           # start from a revision instead of the working tree
ovnfs attach app --to devbox   # the same checkout on another machine, fetched lazily
ovnfs grep 'TODO' app          # search in the daemon, without traversing the mount
ovnfs glob '*.go' app          # matching paths, recursively and ignore-aware
```

`ovnfs help` lists every verb. Verbs start the daemon when none is running;
`start`, `stop`, `restart` and `status` manage it, `serve` runs it in the
foreground. `restart` swaps in the daemon of the binary you run, e.g. after an upgrade.

## How it works

Opening a folder sorts every path into a class. `source` paths are captured:
they are what `diff`, `apply` and snapshots see, and the only bytes that leave
the machine. `cache` and `local` paths (build outputs, ignored files, secrets)
show the live folder, read-only. `hidden` paths are absent. Built-in rules
handle the common cases; `.o2/ovnfs.yml` and `--rule` override them, and
`ovnfs explain PATH` says which rule decided.

Writes go to the checkout's own copy-on-write layer, so the folder is never
written until you `apply`. In a git repository the checkout gets a private git
directory: commit freely, then `apply --branch NAME` to bring the commits back.

ovnfs isolates files, not processes: it is not a sandbox.

## Documentation

- [Configuration](docs/configuration.md): classes, rules, `.o2/ovnfs.yml`, where state and checkouts live
- [Checkouts](docs/checkouts.md): `--rev`, `--take`, `run`, snapshots, forks, `diff` and `apply`
- [Remote checkouts](docs/remote.md): `attach`, `pull`, `share`, the mesh
- [Platforms](docs/platforms.md): NFS, FUSE, SMB and WinFsp frontends
- [Internals](docs/internals.md): admin protocol for embedding, persistence, memory tiers
- [Limitations](docs/limitations.md)
- [Development](docs/development.md): tests and benchmarks
- [Agent skill](skills/ovnfs/SKILL.md): teaches coding agents to work in checkouts; copy `skills/ovnfs` into your agent's skills directory

## License

[MIT](LICENSE)
