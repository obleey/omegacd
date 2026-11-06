# M7 · Hardening

Proving "no data loss" and every supported setup (Q50, Q52).

### M7-01 · Test pyramid with real Agents
**Status:** todo
**Depends on:** M4-05

Unit tests; integration tests running a real Agent against Docker-in-Docker **rootful and rootless**, and **rootless
Podman**; CI runs all of them.

**Done when:** CI is green on all three engines.

### M7-02 · End-to-end suite on Control Plane + Agent
**Status:** todo
**Depends on:** M6-02

Port `test/e2e` (mounts, compose views, secrets, adopt, README walkthrough, webhook) to a Control Plane with an Agent,
and add: enrollment, offline Host, queued Sync, offline rollback, built-in Agent.

**Done when:** `make e2e` is green.

### M7-03 · Chaos tests for no data loss
**Status:** todo
**Depends on:** M7-02

Kill the Control Plane mid-Sync; kill the Source Server; reboot an Agent Host while offline; restore the Control Plane
from a backup and rebuild it from nothing (Q24, Q30, Q31).

**Done when:** in every case all apps run afterwards with their data, and nothing was recreated that didn't change.

### M7-04 · Real-world run
**Status:** todo
**Depends on:** M6-01

Your VPS and a Host behind NAT, installed with `install.sh`, a real stack imported and taken over, a week of normal use.

**Done when:** recorded in TESTING.md with what was found and fixed.

### M7-05 · Documentation
**Status:** todo
**Depends on:** M7-03

README rewritten for v0.2 (concepts, install modes, Agents, Source Server, proxies, security model, upgrade note:
reinstall from v0.1), TESTING.md, ROADMAP.md, SECURITY.md (trust boundaries), ADRs final, screenshots.

**Done when:** a new user installs from the README alone (the v0.1 checklist's test 6, redone).
