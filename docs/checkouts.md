# Checkouts, snapshots and apply

## Git repositories

A checkout of a repository gets a private git directory: HEAD, refs, config and
the index are copies taken at open, and objects borrow the repository's through
alternates. Inside the checkout, `git status` matches the folder's, commits land
in the checkout, and nothing is written to the user's repository until
`ovnfs apply CHECKOUT --branch NAME` fetches the checkout's commits as a branch.

If NAME is checked out in the folder, apply fast-forwards it and updates only
the index entries changed by those commits, never the working-tree files.
Staged changes on those paths or a non-fast-forward are refused; use another
branch name instead. Unrelated staged work stays staged. `--dry-run` previews
this checked-out-branch update without writing.

On a 49k-file repository, opening a checkout takes about a second and
`git status` inside it under three.

A submodule absorbed into the repository's `.git/modules`, as `git submodule
add` and `git submodule update --init` lay it out, gets a private git directory
of its own, built the same way and placed below the checkout's at
`modules/<name>`, where git looks for it; the submodule's `.git` in the checkout
points there. Commits made in the submodule stay in the checkout, forks and
snapshots carry them, and the folder's `.git/modules` is never written. A fork
or a checkout from a snapshot points every submodule `.git` at its own private
git directory, wherever the submodule was moved, and a deinitialized submodule
stays without one.
Submodules absorbed into such a submodule work the same way. Other nested
repositories keep their `.git` as a local copy-on-write directory. A `--rev`
checkout starts with empty submodule directories; `git submodule update --init`
inside it clones them into its private git directory.

Each private gitdir records repo-local `ovnfs.checkout = NAME`, including
forked checkouts with their own names. Existing checkouts gain this marker when
the daemon next opens their registry. The marker is never written to the source
repo.

## Starting from a revision: `--rev`

`ovnfs new [DIR] --rev REV` starts with REV's tracked files and index. A local
branch name (including `refs/heads/NAME`) attaches HEAD to that branch; a
commit, tag or `HEAD` detaches it. Untracked nonignored files disappear from
the checkout, while ignored files and build directories remain available.
Ignore rules come from REV, not the folder's dirty `.gitignore` files. The
source folder and its git state are never changed. `--rev` cannot be combined
with `--from SNAP`; it requires a repository and a revision resolving to a
commit.

The revision is the checkout's base for `diff`, `snap` and `apply`: an
immediate diff is empty, and applying later work does not undo dirty edits
still in the source folder. Revision checkouts substitute tree metadata and
read git blobs lazily, rather than copying or checking out the whole tree.

## Taking paths: `--take`

`ovnfs new [DIR] --take SRC:PATH` replaces a repo-relative file or directory
recursively; repeat it for more selections, with later ones winning. SRC is `.`
for the captured working tree, empty for the staged index, or a revision
resolving to a commit. An empty PATH selects the whole tree. Paths must be
normalized and relative; a path absent from both source and base is an error.
Only source-class paths change: cache, local and hidden paths keep their
ordinary or `--rev` behavior. `--take` cannot be combined with `--from SNAP`.

Taken content becomes part of the base: the initial diff is empty, snapshots
and forks retain it, and apply never writes the selection back to the folder.
Content stays lazy, and the private git index is unchanged: it still comes from
REV with `--rev`, or from the folder otherwise. Git therefore shows taken
content as working-tree changes relative to that index. For example:

```sh
ovnfs new --rev HEAD --take :                  # the staged index alone, without unstaged edits
ovnfs new --rev BASE --take pr-head:path/to/test  # old code with a PR's test ready to run
```

## Disposable runs: `run`

`ovnfs run [DIR] -- CMD [ARGS]...` opens a checkout, runs CMD directly in the
caller's matching subdirectory, and removes the checkout even when CMD fails or
is interrupted. It preserves CMD's output and exit status and sets
`OVNFS_CHECKOUT`; `--keep` retains the checkout and prints its name and path on
stderr. It accepts `--name`, `--rev`, repeatable `--take SRC:PATH` and
`--rule LINE`; `--` is required.

`--on HOST [--remote-bin PATH]` instead runs over ssh and keeps both checkouts
as a lane for [`pull`](remote.md#pull). Its remote name defaults to the
folder's name, so repeated runs reuse that remote lane. It attaches the folder
directly unless `--rev`, `--take` or `--rule` requires opening a local checkout
first.

## Snapshots and forks

`snap` renames the upper into an immutable layer in O(1) and hashes its source
paths into a git-format tree in the background (`--wait` blocks); hashing
touches only the changed paths, and the store keeps a changed file as a delta
against its previous version, so a snapshot costs about what changed. `fork`
snapshots a checkout and opens a second one on that snapshot; parent and child
diverge. `new --from SNAP` opens a checkout on any snapshot. `rm` removes a
checkout and keeps its snapshots; `drop` releases one. Nothing replaces the
content of a live folder: rollback means a new checkout from a snapshot.

`new` (and `run`) on a checkout's own folder forks that checkout: capturing it
would make the daemon serve the new checkout by reading through its own mount,
which can deadlock the mount. `new` refuses the mount itself, a folder below a
checkout's root, and `--rev`, `--take` or `--rule` with a checkout.

## Search: `grep` and `glob`

`grep PATTERN [DIR|CHECKOUT]` searches contents; `glob PATTERN [DIR|CHECKOUT]`
lists matching paths. Both run in the daemon. A named checkout, its mounted
path, or a directory below that path reads the merged overlay directly,
including memory-resident writes, frozen layers and live cache/local paths.
There is no recursive traversal through NFS, FUSE or SMB. Other folders use
the daemon's native filesystem. Omit the target to search the current directory.

```sh
ovnfs grep 'func\s+New' app --glob '*.go' -C 2
ovnfs glob '*_test.go' app --limit 100
ovnfs --json glob '*.go' app --mtime
ovnfs grep 'needle' app --path src --mode filesWithMatches
```

Patterns without a slash match at any depth; `--anchored` restricts them to
the search root. Ignore files are honored, dot-prefixed paths and
`node_modules` are skipped, and `.git` is always pruned. `--no-ignore`,
`--hidden` and `--node-modules` override those respective traversal defaults;
a pattern naming `node_modules` also enables it. Hidden-class paths remain
absent regardless of these flags. Symlink directories are not descended.

Grep supports `-F` literal text, `-i`, `--multiline`, file `--type` filters,
`content`, `filesWithMatches` and `count` modes, context (`-A`, `-B`, `-C`),
`--offset`, `--max-count`, `--max-count-per-file` and `--max-columns`.
Grep's `--glob` also searches every file under a directory it matches
(`'{src,test}'`, `'packages/*/src'`), while `'dir/*.ts'` stays limited to
`dir`'s direct children. Repeatable `--exclude GLOB` skips matching files
and does not enter matching directories; a slash-free exclude matches at any
depth and a leading `/` anchors it to the search root. Glob patterns take
`\` as `/` and close unclosed `{` groups. `--max-columns` cuts context lines
to their start but windows a long matching line around its first match,
reporting that match's character `column` in JSON.
Specifying either `-A` or `-B` replaces both sides of `-C`. Each file is
searched over its first 4 MiB by default; `--max-file-size` changes the
window, and JSON reports files whose tails were skipped. The regex dialect
retains gogrep's literal repairs for malformed patterns; unsupported
constructs such as lookaround return an error.

Glob supports `--type file|dir|symlink`, `--limit`, and `--mtime` for
newest-first results with metadata. Both commands accept `--workers`,
`--max-depth` and a root-relative `--path`. Results have deterministic path
order unless glob sorts by time. Empty results succeed.

In a checkout [attached from another machine](remote.md#search), grep scans
the files that machine still holds there and moves only results; see the
remote documentation for what crosses the link.

Ctrl-C cancels daemon-side search work, not just the CLI's wait. Embedded Go
clients cancel an individual `admin.Client.Call` context or set its deadline;
other calls on the connection remain usable. Canceled searches return an
error, not a successful partial result. Cancellation is cooperative: an
in-progress filesystem call, lock acquisition or regexp operation must finish
before worker cleanup can complete.

The same `grep` and `glob` operations are available over the admin socket;
[`api/search.go`](../api/search.go) defines their arguments and JSON replies.
Replies above the protocol's 16 MiB frame limit return `too_large`; use
result limits, narrower patterns or grep's column limit to reduce the reply.

## Diff and apply

`diff CHECKOUT [SNAP] [-p]` compares the checkout now with its base capture (or
a snapshot). `apply CHECKOUT` snapshots the checkout and merges each changed
source path into the folder against the base: a path the user left alone takes
the checkout's version, and a text file changed on both sides is merged with
`git merge-file`.

Where both sides changed the same lines, the file gets git-style conflict
markers (`<<<<<<< folder` … `>>>>>>> checkout NAME`, in your
`merge.conflictStyle`) and apply exits 1, asking you to resolve them and run it
again. Until the markers are gone, apply reports the file again and leaves it
alone. Once resolved, the checkout's later edits to that file merge against the
version that was marked, so the resolution is kept, across daemon restarts too.
Anything else (deletion against an edit, binary or symlink changes on both
sides) is reported as a conflict and left untouched. Applying twice is a no-op.
`--dry-run` writes nothing.
