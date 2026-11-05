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
- A link is one stream per checkout (QUIC over the mesh, or ssh); cold reads of
  many files are bound by its throughput.
- The mesh does not move a relayed connection to a direct path in place (the
  Rust nodes do that with noq's multipath and NAT-traversal extensions, which
  quic-go lacks): it races direct ways first when it connects and re-dials
  when a connection ends or stalls (three seconds without hearing the peer).
  Two machines that can only see each other through NAT stay on the relay.
- Membership sync is push on change plus one round at daemon start; a member
  offline during a change learns it at its next start or the next change.
- NFSv3 locks are client-local. Explicit NFSv4 clients use byte-range locks
  held in the daemon's memory, lost when it restarts. SMB has no hard links
  and no durable handles.
- NFSv3 cannot push cache invalidations: folder edits in cache and local
  paths, `pull`, and reattach can remain cached until the client's attribute
  timeout (up to 60 s). NFSv4 read delegations reduce that window for opened
  files, but do not cover directories or files only inspected with `stat`.
- WinFsp mounts get no recalls or change notifications for changes made
  behind them (`pull`, reattach, folder edits): they show once the one-second
  metadata cache expires, and directory watchers miss them. Neither WinFsp
  nor SMB creates directory symlinks.
