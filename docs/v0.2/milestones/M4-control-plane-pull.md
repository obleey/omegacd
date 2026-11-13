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

### M4-04 · The Control Plane's machine through a normal Agent
**Status:** done (2026-11-11, PR #124)
**Depends on:** M4-01

First a built-in Agent inside `omegacd server`; dropped (Q104): the Control Plane's machine is a Host like any other,
its Agent installed beside the Control Plane and enrolled with an invite. What every Agent needs stays: it finds its
engine's socket when `DOCKER_HOST` is unset and checks it when set, before the Docker client (which exits on an address
it can't use) sees it (Q103); `adopt` may be set on an existing Application, the
Agent checking at its first deploy (Q102); a Diff through an Agent keeps the Source Server's warnings.

**Done when:** the e2e scripts that deploy to `local` (mounts, compose views, secrets, adopt, migrations, webhook,
source server) enroll an Agent as any Host does (`test/e2e/enroll.sh`) and deploy through it, each checking it in the
Agent's log, updated where they look at the old path (the Control Plane's work dir). The README walkthrough follows
with M4-05, which rewrites the README for Hosts without `local`.

### M4-05 · Delete push and legacy
**Status:** done (2026-11-12, PR #125)
**Depends on:** M4-01, M4-02, M4-04

Remove (Q9, Q18, Q104): SSH/TLS Hosts and their settings, the Host `local` and the default Host (every Application
names its `host:`), the Control Plane's own deploy path and engine, `OMEGACD_LOCAL_HOST`, central decryption
and stored private Host Keys (the Control Plane keeps Agents' public keys only), the old layouts and their fallbacks,
`import --ssh`, `host key generate|import|remove`, and the image's `openssh-client` (the Dockerfile installs it only
for the Docker client's `ssh://` Hosts; Git over SSH is go-git). `OMEGACD_SSH_KNOWN_HOSTS` stays, for Git over SSH only
(Q106). One public Host Key recipient per Host, from its Agent (Q108). A fresh v0.2 database schema; no migration
(Q105). The UI parts that call removed endpoints go in the same PR, with no new UI (Q110).
Decided before it: Q105–Q110.

A v0.1 database is refused with a message (export with v0.1's `app export`, then reinstall) and left untouched (Q90).

From M4-04: the check that OmegaCD doesn't Sync its own Compose project (`refuseSelf`, today the Control Plane's)
moves into the Agent, which refuses only the project containing its own container (Q109): the Control Plane's is an
Application like any other, OmegaCD managing itself (Q104). The README is edited to be correct and its walkthrough, with
its e2e script, installs and enrolls an Agent first; the deploy files lose the socket, the work dir and
`OMEGACD_LOCAL_HOST` (Q110).

**Done when:** none of it is left (grep), all tests pass, the binary is smaller, and a v0.1 database is refused
without being changed.

### M4-06 · History retention and disaster recovery
**Status:** done (2026-11-13, PR #127)
**Depends on:** M4-01, M3-06

History is kept forever by default with optional retention by count or age (Q36). Disaster recovery (Q30): a
re-enrolled Agent reports what it runs; apps unknown to the Control Plane show as "on the Host, not managed" and are
never touched; an App of Apps from Git takes them over by name without redeploying unchanged ones.

Settled before it (Q112–Q119): retention by `OMEGACD_HISTORY_KEEP` and `OMEGACD_HISTORY_MAX_AGE`; `omegacd backup`
(the database and the encrypted Git credentials, never a key), restored by documented steps; the Master Key generated
when missing; Sync Operations a restored database still shows Running closed as unknown; an enrolled Agent's outbox
cleared; the Applications a Host runs that the Control Plane doesn't know listed per Host; a takeover at the same
Revision needs no Sync Operation.

**Done when:** tested from a backup and from an empty Control Plane, with no container recreated that didn't change.

### M4-07 · Serve delivered payloads without the Source Server
**Status:** done (2026-11-13, PR #128)
**Depends on:** M4-01

With the Source Server down (Q31): reinstalled Agents and Rollbacks to rendered Revisions are served from the
Control Plane's blob store; new Revisions wait with the reason shown. The store prunes with the history (Q113): a payload goes when no kept
Sync Operation uses it.

Settled before and while building it (Q120–Q128): each render's file list kept by tree checksum, a Sync deploying
from it; the files copied into `<data>/payloads` before a Desired State goes; Rollback and Syncs that waited deploy
the kept render; an Agent that doesn't run an Application deployed on its Host gets it again (`restore`); a new
Revision refused with 503 and the reason, no Sync Operation; payloads pruned hourly with the history; a reinstalled
Agent comes back by its backed-up keys (no way back for a Host with Applications whose keys are lost); the gRPC client
finds a restarted Source Server within seconds.

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
**Status:** split into M4-10a–e (Q129); closed by M4-10e
**Depends on:** M4-09, M4-05 (Q97: v0.1-only code is deleted, not tested)

Q96: every package's reachable lines covered, unit + integration + e2e combined; what no test can reach is marked in the code with
its reason. The same pass checks logging (Q62): every error path logs or returns to a caller that logs, every
decision logs at debug, nothing fails silently; where the log is what tells an operator what happened, the path's
test checks the log line. Code M4-05 deletes (push, SSH/TLS Hosts, the local engine path) is left
out: it is deleted first, never tested (Q97). Then CI requires 100%: any uncovered line not in the exceptions fails.

How (Q129–Q135): one sub-ticket per group of packages, each to 100% and no silent path; tested where a test reaches it
cheaply, marked only where none can (Q130); decisions at debug (Q131); operator-facing Warn and Error lines asserted
(Q132); the Go code only (Q135). At the start: 713 uncovered blocks, 88.0%.

**Done when:** `make cover` shows 100% of the lines not marked unreachable, CI enforces it, and no error path is
silent (reviewed package by package, the operator-facing ones tested by their log line).

### M4-10a · The review's findings, shutdown order; store, blob, crypto, wire, logging, domain, version to 100%
**Status:** done (2026-11-13, PR #134)
**Depends on:** M4-09

From M4-06: shutting down with Agents connected logs `ERROR record the agent leaving: sql: database is closed`: the
store closes before the hub's sessions end (hijacked connections outlive the HTTP server's Shutdown). The server waits
for its Agent sessions before closing the store.

From a review of the wiring before M4-10 (2026-11-13), settled as Q134:
- The Control Plane's database opens one with a newer schema: `store.migrate` runs only what it lacks, so an older
  binary starts silently on a schema it doesn't know. Refuse it, as the Agent's database does ("upgrade").
- The Agent takes Application and App of Apps names from the Control Plane into its folder path
  (`root/<parent>/<name>`) unchecked; only `root` is checked (Q57). It checks them with the Control Plane's own rule.
- An unset `OMEGACD_WEBHOOK_SECRET` lets anyone trigger refreshes (README) but the server says nothing: warn at start.
- Silent paths: the Agent drops
  `ListApps` errors in `Watch` and in `keepInSync`'s first reports; `agentDetail` drops `AgentStatus` errors; `emit`
  drops events for a slow subscriber unlogged; `omegacd import` falls back to `main` unsaid; the importer reads a
  `du` size it can't parse as 0; `pendingChildren` ignores `childDiff` errors.

**Done when:** the findings are fixed with tests; `internal/store`, `blob`, `crypto`, `wire`, `logging`, `domain`,
`version` and `secretkey` are at 100% (or marked, Q130) with no silent path.

### M4-10b · source, workdir, secrets, appdef to 100%
**Status:** todo
**Depends on:** M4-10a

**Done when:** `internal/source`, `workdir`, `secrets` and `appdef` are at 100% (or marked, Q130) with no silent path,
the Source Server's operator-facing Warn and Error lines tested (Q132).

### M4-10c · agent, agents, engine to 100%
**Status:** todo
**Depends on:** M4-10a

**Done when:** `internal/agent`, `agents` and `engine` are at 100% (or marked, Q130) with no silent path, the Agent's and
the hub's operator-facing Warn and Error lines tested (Q132).

### M4-10d · controller, api, cmd/omegacd to 100%
**Status:** todo
**Depends on:** M4-10a

**Done when:** `internal/controller`, `api` and `cmd/omegacd` are at 100% (or marked, Q130) with no silent path, the
server's operator-facing Warn and Error lines tested (Q132).

### M4-10e · cli, importer to 100%; CI requires 100%
**Status:** todo
**Depends on:** M4-10b, M4-10c, M4-10d

**Done when:** `internal/cli` and `importer` are at 100% (or marked, Q130); CI's Coverage job and `make gate` fail on any
uncovered block not marked (`-all`), floor 100 (Q133); closes M4-10.

### M4-12 · Ubuntu 26.04: Podman green
**Status:** deferred: after v0.2 (Q111, 2026-11-12; ROADMAP)
**Depends on:** M4-05
**Issue:** [#126](https://github.com/obleey/omegacd/issues/126)

CI ran on `ubuntu-latest`. When GitHub moved it to Ubuntu 26.04, the current LTS (PR #125), the runner's own rootless
Podman failed every container removal: `rootless netns: kill network process: permission denied`. Rootful and
rootless Docker and Podman in a container passed. The cause isn't known yet (probably AppArmor and pasta). v0.2 is
tested on Ubuntu 24.04 only: every CI job is pinned to `ubuntu-24.04` (PR #125). After v0.2.0, to do:
- find the cause; fix it in the Agent if an Agent can hit it, otherwise in the tests' setup, and document what a Host
  needs;
- CI's jobs on `ubuntu-26.04`, Agent engines green there;
- Ubuntu 26.04 machines (amd64 and arm64) in `make test-linux` (omegacd-testinfra) and M7-07's runner labels; Q61 and
  TESTING.md name 26.04 as tested.

**Done when:** CI is green on `ubuntu-26.04` and `make test-linux` on Ubuntu 26.04 on both architectures,
with the runner's own Podman.
