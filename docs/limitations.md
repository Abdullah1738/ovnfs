# Limitations

- This is not a sandbox: absolute paths and symlinks can reach the host.
- The view is best effort, not a frozen snapshot: a source file the user
  rewrites while the agent is reading it can show either version, and cache
  and local paths always show the folder as it is.
- A tracked file the user changes after open is read from the repository's
  objects; pruning those objects can make it unreadable.
- Remote checkouts have no git directory and cannot be attached onward. Bring
  their edits back with `pull` on the source machine, then `apply` the local
  checkout to its folder. `fstat` on a lazily fetched open file reports the
  cached blob's metadata; path-based stat is exact.
- A link is one ssh session per checkout; cold reads of many files are bound by
  its throughput.
- NFSv3 locks are client-local. Explicit NFSv4 clients use byte-range locks
  held in the daemon's memory, lost when it restarts. SMB has no hard links
  and no durable handles.
- A file's id derives from its directory and name. Within one daemon run a
  removed file's id is never reissued, but after a restart a file recreated
  under the same name can take the id an NFS client still holds for the
  removed one; a hash collision with a name created behind the export while
  the daemon was down makes an earlier handle stale (`ESTALE`), never another
  file's.
- NFSv3 cannot push cache invalidations: folder edits in cache and local
  paths, `pull`, and reattach can remain cached until the client's attribute
  timeout (up to 60 s). NFSv4 read delegations reduce that window for opened
  files, but do not cover directories or files only inspected with `stat`.
- One SMB daemon per Windows logon session: the SMB client keeps one set of
  credentials per server name, whatever the port, so a second daemon, whose
  state directory holds another password, cannot map `\\127.0.0.1\ovnfs`
  (error 1219). Run further daemons with `-proto winfsp`.
- WinFsp mounts get no recalls or change notifications for changes made
  behind them (`pull`, reattach, folder edits): they show once the one-second
  metadata cache expires, and directory watchers miss them.
