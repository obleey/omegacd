# M4 · Control Plane in pull mode

The Control Plane stops talking to Docker and drives Agents instead; push is deleted.
See [architecture](../architecture.md#control-plane-q9-q23-q26-q30-q31-q35-q36-q38-q41-q46-q49).

### M4-01 · Sync through Agents
**Status:** todo
**Depends on:** M1-03, M3-03

Sync = Source Server Render → blobs stored (kept, Q31) → desired state sent to the Host's Agent → Sync Operation
from the Agent's reports. Queued Syncs for offline Hosts ("waiting for Host"); a newer Sync replaces a queued one
(Q38). Auto-Sync, Self-Heal, retries, Prune and Rollback keep today's semantics. The Desired State carries the
Application's work dir (Q57, M2-09), and the Sync Operation's ID: the Agent's log entries for it carry the same `op`
(Q62, M2-10).

**Done when:** the controller unit tests pass with a fake Agent, and an integration test syncs, rolls back and queues
against a real Agent; the e2e follows one Sync by its `op` from the Control Plane's log into the Agent's.

### M4-02 · Status from Agents, last-known state offline
**Status:** todo
**Depends on:** M4-01

Sync Status from target vs deployed Revision plus the Agent's drift report; Live State, Diff, Health, resources and
logs from the Agent (Q23). Offline: the last known values with their time, "Host offline since …" (Q38). Disconnected
Hosts no longer make apps Unknown just because they're offline.

**Done when:** the UI's views work unchanged against Agent-reported data, and an offline Host shows last-known state.

### M4-03 · Polling, webhooks and App of Apps through the Source Server
**Status:** todo
**Depends on:** M1-03

Polling asks the Source Server to Resolve; webhooks are forwarded as "repository changed"; App of Apps definitions
come from Render (Q46). Every child names its Host (already so since rc.5).

**Done when:** the Control Plane has no Git code path left (test: no import of the git library outside the Source Server).

### M4-04 · Built-in Agent
**Status:** todo
**Depends on:** M4-01

`omegacd server` starts an Agent in-process when `OMEGACD_BUILTIN_AGENT=auto|true` (auto: a Docker-compatible socket
is reachable and `KUBERNETES_SERVICE_HOST` is unset), enrolled automatically, otherwise a normal Agent; the startup log
says on or off and why (Q26, Q35).

**Done when:** a full install on a Docker host manages its own machine with no enrollment step; in Kubernetes it's off.

### M4-05 · Delete push and legacy
**Status:** todo
**Depends on:** M4-01, M4-02, M4-04

Remove (Q9, Q18): SSH/TLS Hosts and their settings, the remote file helper, `OMEGACD_LOCAL_HOST`, central decryption
and stored private Host Keys (the Control Plane keeps Agents' public keys only), the old layouts and their fallbacks,
`import --ssh`. A fresh v0.2 database schema; no migration.

**Done when:** none of it is left (grep), all tests pass, and the binary is smaller.

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
