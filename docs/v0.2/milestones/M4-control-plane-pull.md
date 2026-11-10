# M4 · Control Plane in pull mode

The Control Plane stops talking to Docker and drives Agents instead; push is deleted.
See [architecture](../architecture.md#control-plane-q9-q23-q26-q30-q31-q35-q36-q38-q41-q46-q49).

### M4-01 · Sync through Agents
**Status:** done (2026-11-09, PR #113)
**Depends on:** M1-03, M3-03

Sync = Source Server Render → blobs stored (kept, Q31) → desired state sent to the Host's Agent, which asks for the
blobs it lacks → Sync Operation from the Agent's reports. Agents sit behind the controller's engine seam; push Hosts
and `local` keep working until M4-05 and M4-04 (Q80). A Sync to an offline Host is a Sync Operation "Waiting for
Host", kept in the database; a newer Sync supersedes it (Q38, Q81); delivering it on reconnect is M3-06's resync.
Auto-Sync, retries, Prune and Rollback keep today's semantics; Self-Heal runs on the Agent, online too, its runs
recorded as Sync Operations with trigger `self-heal` (Q82). A Sync whose connection drops finishes on the Agent and
shows "Running, Host disconnected" until its result arrives (Q93). The Desired State carries the Application's work
dir (Q57, M2-09), and the Sync Operation's ID: the Agent's log entries for it carry the same `op` (Q62, M2-10). The
Agent trusts the Control Plane only over TLS (loopback http allowed), `OMEGACD_AGENT_CA_FILE` for a private CA (Q95).

**Done when:** the controller unit tests pass with a fake Agent, and an integration test syncs, rolls back and waits
for an offline Host against a real Agent; the e2e deploys an Application from Git through an enrolled Agent and
follows one Sync by its `op` from the Control Plane's log into the Agent's; an Agent refuses an `http://` Control
Plane that isn't on loopback.

### M4-02 · Status from Agents, last-known state offline
**Status:** done (2026-11-09, PR #120)
**Depends on:** M4-01, M4-08

The Agent reports each Application's Health, Live State (containers), drift and resources whenever they change
(engine events, after a Sync, on connect) (Q23); the Control Plane keeps the latest report with its time. Sync Status
from target vs deployed Revision plus the reported drift; the UI's views (Health, Live State, resources, drift) read
the reports. Offline: the last known values with their time, "Host offline since …" (Q38); disconnected Hosts no
longer make apps Unknown just because they're offline. Logs come from the Agent on demand, `-f` included, while it is
connected. `app sync --wait` waits for Health again on Agent Hosts. The Diff of a new Revision is M4-11 (Q98).

**Done when:** the UI's views (except a new Revision's Diff) work unchanged against Agent-reported data; an offline
Host shows the last-known state with its time; logs stream from an Agent; `--wait` waits for Health; e2e through the
binary for each.

### M4-03 · Polling, webhooks and App of Apps through the Source Server
**Status:** done (2026-11-10, PR #122)
**Depends on:** M1-03

Polling asks the Source Server to Resolve; webhooks are forwarded as "repository changed"; App of Apps definitions
come from Render (Q46). Every child names its Host (already so since rc.5).

A generic webhook endpoint takes any other caller (Q99): `POST /api/webhooks/generic`, the secret as a bearer token,
`{"repoURL": "…"}`.

**Done when:** the Control Plane has no Git code path left (test: no import of the git library outside the Source
Server), and end to end, with a standalone Source Server, an App of Apps' children reach an Agent, a push reaches it
through a provider's webhook and through the generic one, and through polling.

### M4-04 · Built-in Agent
**Status:** done (2026-11-10, PR #124)
**Depends on:** M4-01

`omegacd server` starts an Agent in-process when `OMEGACD_BUILTIN_AGENT=auto|true` (auto: a Docker-compatible socket
is reachable and `KUBERNETES_SERVICE_HOST` is unset), enrolled automatically, otherwise a normal Agent; the startup log
says on or off and why (Q26, Q35).

Its Host is `local`, its state dir in the data dir, its work dir `OMEGACD_WORKDIR` (Q88); it may run as the Control
Plane's user, root included, with a warning (Q89).

It has its own Host Key, and is enrolled and approved as `local` at every start (Q100); off, `local` deploys as before
until M4-05 (Q101).

**Done when:** a full install on a Docker host manages its own machine with no enrollment step; in Kubernetes it's off;
and the e2e scripts that deploy to `local` (mounts, secrets, webhook, adopt, migrations, …) deploy through the built-in
Agent, which each checks in its Agent's log, updated where they look at the old path (the Control Plane's work dir).

### M4-05 · Delete push and legacy
**Status:** todo
**Depends on:** M4-01, M4-02, M4-04

Remove (Q9, Q18): SSH/TLS Hosts and their settings, the remote file helper, `OMEGACD_LOCAL_HOST`, central decryption
and stored private Host Keys (the Control Plane keeps Agents' public keys only), the old layouts and their fallbacks,
`import --ssh`, and with them the image's `openssh-client` (the Dockerfile installs it only for the Docker client's
`ssh://` Hosts; Git over SSH is go-git) and `OMEGACD_SSH_KNOWN_HOSTS` with its known_hosts handling. A fresh v0.2
database schema; no migration.

A v0.1 database is refused with a message (export with v0.1's `app export`, then reinstall) and left untouched (Q90).

From M4-04: the check that OmegaCD doesn't Sync its own Compose project (`refuseSelf`, today the Control Plane's, run
for `local` even through the built-in Agent) moves into the Agent, which knows its own container; and with the
built-in Agent off there is no `local` Host any more (Q101).

**Done when:** none of it is left (grep), all tests pass, the binary is smaller, and a v0.1 database is refused
without being changed.

### M4-06 · History retention and disaster recovery
**Status:** todo
**Depends on:** M4-01, M3-06

History is kept forever by default with optional retention by count or age (Q36). Disaster recovery (Q30): a
re-enrolled Agent reports what it runs; apps unknown to the Control Plane show as "on the Host, not managed" and are
never touched; an App of Apps from Git takes them over by name without redeploying unchanged ones.

**Done when:** tested from a backup and from an empty Control Plane, with no container recreated that didn't change.

### M4-07 · Serve delivered payloads without the Source Server
**Status:** todo
**Depends on:** M4-01

With the Source Server down (Q31): reinstalled Agents and Rollbacks to rendered Revisions are served from the
Control Plane's blob store; new Revisions wait with the reason shown.

**Done when:** tested with the Source Server stopped.

### M4-08 · Deletes, registry credentials and live progress through Agents
**Status:** done (2026-11-09, PR #116)
**Depends on:** M4-01, M3-06

Split from M4-01 (Q94). Deleting goes through the Agent (`RemoveApp`: delete volumes, keep containers), so `app
delete`, `--volumes` and `--cascade=false` keep their meaning; to an offline Host it waits, the Application showing
"Deleting, waiting for Host" until its Agent confirms (Q83). `app delete --forget` (and the UI's dialog) drops an
Application whose Host is gone for good, after a confirmation (Q92). Registry credentials stay on the Control Plane;
the Agent asks for a registry's when it pulls from it and gets them age-encrypted to its Host Key, decrypted in memory
for that pull only: no plaintext on the wire, on a disk or in a log (Q84–Q86). Live Sync progress is streamed by the
Agent, so the UI's progress view keeps working (Q87).

**Done when:** e2e through an Agent: delete with and without volumes, keep-containers, a delete waiting for an offline
Host and done on reconnect, `--forget`; an image from a private registry pulled with credentials that appear in no
log, on no disk of the Agent and never in plaintext on the wire (checked by capturing it); the UI's live progress fed
by an Agent.

### M4-11 · The Diff of a new Revision on the Agent
**Status:** done (2026-11-09, PR #121)
**Depends on:** M4-02

Split from M4-02 (Q98). The Diff view for a Revision not yet deployed: the Control Plane sends its Desired State; the
Agent fetches the files it lacks, stages them in its work dir (RAM by default: decrypted values never on disk, Q19),
decrypts and diffs against what runs, without deploying; the staged folder is removed. Offline: the last Diff it
reported, with its time.

**Done when:** the UI's Diff of a new Revision works against an Agent unchanged, nothing is deployed or left behind,
no decrypted value is in the Control Plane's data or logs; e2e through the binary.

### M4-09 · Coverage: measured combined, checked in CI
**Status:** done (2026-11-09, PR #118)
**Depends on:** M4-08

Q96: `make cover` builds the binary with `go build -cover`, runs unit (`-coverprofile`), integration and e2e (the
binary's `GOCOVERDIR`; the e2e scripts pass it to every process they start) and merges them (`go tool covdata`),
generated protobuf code excluded, into one total, a per-package table and the list of uncovered lines.
Lines no test can reach are marked in the code (`coverage: unreachable, <reason>`). CI runs it on every push and
fails when a changed line is uncovered and not an exception, or the total drops (until M4-10 makes it 100%).
TESTING.md documents it.

**Done when:** `make cover` prints the combined total and every uncovered line; CI fails a PR that leaves a changed
line uncovered and unlisted, or lowers the total (tested with a throwaway change).

### M4-10 · Existing code to 100% coverage, and no silent path
**Status:** todo
**Depends on:** M4-09, M4-05 (Q97: v0.1-only code is deleted, not tested)

Q96: every package's reachable lines covered, unit + integration + e2e combined; what no test can reach is marked in the code with
its reason. The same pass checks logging (Q62): every error path logs or returns to a caller that logs, every
decision logs at debug, nothing fails silently; where the log is what tells an operator what happened, the path's
test checks the log line. Code M4-05 deletes (push, SSH/TLS Hosts, the local engine path) is left
out: it is deleted first, never tested (Q97). Then CI requires 100%: any uncovered line not in the exceptions fails.

**Done when:** `make cover` shows 100% of the lines not marked unreachable, CI enforces it, and no error path is
silent (reviewed package by package, the operator-facing ones tested by their log line).
