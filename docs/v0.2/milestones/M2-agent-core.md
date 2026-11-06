# M2 · Agent core

`omegacd agent` deploying Applications on its own Host, before it talks to anyone: Compose, cache, decryption,
work dir, offline autonomy. See [architecture](../architecture.md#agent-q3-q4-q10-q19-q21-q24-q25-q29-q32-q34-q48-q52).

### M2-01 · `omegacd agent` command and configuration
**Status:** done (2026-11-06, PR #76)
**Depends on:** —

The `agent` persona (Q10): configuration from environment variables (Control Plane URL, invite, state dir, work
dir, `DOCKER_HOST`, cache size, secret source settings). Refuses to run as root unless `OMEGACD_AGENT_ALLOW_ROOT=true`.
State dir defaults: `$XDG_DATA_HOME/omegacd-agent` (user) or `/var/lib/omegacd-agent` (system service).

**Done when:** `omegacd agent --help` documents every setting, root is refused with a clear message, and a unit test
covers the defaults for user vs system service.

### M2-02 · Agent local database
**Status:** done (2026-11-06, PR #78)
**Depends on:** M2-01

SQLite in the state dir (Q48): Applications on this Host (name, deployed Revision, file list, settings it needs
offline such as Self-Heal), the cache index, and an **outbox** of events to report on reconnect (restarts,
Self-Heal repairs, offline rollbacks).

**Done when:** schema with migrations from day one, and tests for outbox ordering and survival across restarts.

### M2-03 · Revision cache
**Status:** done (2026-11-06, PR #79)
**Depends on:** M1-04, M2-02

The content-addressed cache (Q27, Q32): ciphertext only, last 10 Revisions per app (`OMEGACD_AGENT_CACHE_REVISIONS`),
blobs shared across Revisions and apps.

**Done when:** retention keeps exactly the configured Revisions plus the deployed one, never a plaintext file in the
cache (test scans it), and a Rollback to a cached Revision needs no download.

### M2-04 · Work dir writer and ownership
**Status:** done (2026-11-06, PR #80)
**Depends on:** M2-03

Write an app's files into `<work dir>/<name>` (or `<work dir>/<parent>/<name>`) **in place** (keep inodes for
single-file bind mounts) and prune everything else (Q11, Q19, Q48). Move the folder ownership rules from today's
`internal/controller/workdir.go` and the remote helper here unchanged. Work dir on RAM or disk as configured; in a
container, check path parity at start (Q21).

**Done when:** today's workdir tests pass against the Agent, an inode test proves in-place writes, a foreign folder
is refused, and a container without path parity refuses to start with an explanation.

### M2-05 · Secret sources and decryption in memory
**Status:** todo
**Depends on:** M2-04

A `SecretSource` interface on the Agent (Q29) with SOPS+age as the only v0.2 implementation. The Agent generates its
age Host Key at first start (Q4). Decryption reads ciphertext from the cache and writes plaintext **only** into the
app's folder, never to a temp file (Q19). The decrypted-file list and key names are reported for the UI's masked view.
Decrypted files are `0600`, owned by the Agent's user; with `OMEGACD_AGENT_SECRET_GROUP=<gid>` they are `0640` with
that group, for non-root containers that join it with `group_add:` (Q58). `omegacd agent --help` and the docs say
which containers can read them (root yes; a non-root user only through the group).

**Done when:** a test proves no plaintext touches any path but the app folder, SOPS files from v0.1's format still
decrypt, a second (dummy) source can be registered without changing callers, and an e2e shows a non-root container
refused a `0600` secret file and reading it with `OMEGACD_AGENT_SECRET_GROUP` and `group_add:`.

### M2-06 · Compose on the Agent
**Status:** todo
**Depends on:** M2-04, M2-05

Reuse `internal/engine` locally: Sync, Diff, Status, Health, events/drift, logs, delete (with and without volumes),
Prune. Detect and report the engine (Docker/Podman), version, rootless (Q34). Final Compose render (with decrypted
`.env`) happens here (Q12).

**Done when:** the engine integration tests run through the Agent against rootful Docker, rootless Docker and
rootless Podman.

### M2-07 · Offline autonomy and restore
**Status:** todo
**Depends on:** M2-06

With no Control Plane (Q24): restore apps after a reboot from the cache (RAM work dir), run Self-Heal from the last
Desired State, never deploy new Revisions, never delete or stop an app because the Control Plane is gone; record
everything in the outbox.

**Done when:** tests reboot (wipe the RAM work dir) with no Control Plane and get every app back with its files, and
a stopped container is repaired offline when Self-Heal is on.

### M2-08 · Offline rollback on the Host
**Status:** todo
**Depends on:** M2-07

`omegacd agent rollback APP REVISION` to any cached Revision (Q25), listing cached Revisions without arguments;
recorded in the outbox for the Control Plane to show as rolled back on the Host and turn Auto-Sync off.

**Done when:** works with the Control Plane unreachable, refuses uncached Revisions with the list of cached ones,
and the outbox entry is produced.

### M2-09 · Per-Application work dir
**Status:** todo
**Depends on:** M2-04

An Application may name its own work dir (Q57): `workDir:` (absolute) in its definition; empty means the Host's
work dir, so the folder stays `<OMEGACD_AGENT_WORKDIR>/<name>`. Otherwise the folder is `<workDir>/<name>` (or
`<workDir>/<parent>/<name>`); a Child Application sets its own in its definition, nothing is inherited from its
App of Apps. A root that doesn't exist yet is created like the Host's work dir. The Agent accepts a work dir only under a root in `OMEGACD_AGENT_WORKDIR_ALLOW`
(comma-separated absolute paths) or its own work dir, and refuses others in the Sync with a message naming the
setting. Ownership rules apply in every root; in a container every allowed root needs path parity, checked at
start. Changing an Application's work dir moves its files like changing its App of Apps does. The field travels in
the Desired State (protobuf) and the Application (definition, database, API) here; the UI field is M5-07.

**Done when:** an Application in a second allowed root deploys there, one outside every allowed root is refused
without a file written, moving between roots leaves no folder behind, and an allowed root without path parity
stops a containerised Agent at start, each with unit and e2e tests.
