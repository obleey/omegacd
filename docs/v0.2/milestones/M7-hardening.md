# M7 · Hardening

Proving "no data loss" and every supported setup (Q50, Q52).

### M7-01 · Test pyramid with real Agents
**Status:** todo
**Depends on:** M4-05

Unit tests; integration tests running a real Agent against Docker-in-Docker **rootful and rootless**, and **rootless
Podman**; CI runs all of them. M2-06 builds this matrix for the Agent alone; here it grows to the Control Plane
driving the Agent.

**Done when:** CI is green on all three engines.

### M7-02 · End-to-end suite on Control Plane + Agent
**Status:** todo
**Depends on:** M6-02

Port `test/e2e` (mounts, compose views, secrets, adopt, README walkthrough, webhook) to a Control Plane with an Agent,
and add: enrollment, offline Host, queued Sync, offline rollback, OmegaCD managing its own Control Plane through its machine's Agent (Q104).

Open: CI could run this whole gate on the Hetzner test servers too (the API token as a GitHub secret; up, test, down
per run), so Debian is tested in CI and pull requests from people who don't run the gate are covered: today GitHub's
runners are Ubuntu only and Debian is tested by the local gate before every push. Decide when outside contributions
start (a Debian container on the runners was rejected: no systemd or Docker of its own there, only part of the suite).

**Done when:** `make e2e` is green.

### M7-03 · Chaos tests for no data loss
**Status:** todo
**Depends on:** M7-02

Kill the Control Plane mid-Sync; kill the Source Server; reboot an Agent Host while offline; restore the Control Plane
from a backup and rebuild it from nothing (Q24, Q30, Q31).
Damage a Source Server's repo cache (`<data>/repos/<hash>`): it deletes and clones it again on its own (Q139; until
then its errors name the folder to delete).

**Done when:** in every case all apps run afterwards with their data, and nothing was recreated that didn't change.

### M7-04 · Real-world run
**Status:** todo
**Depends on:** M6-01

Your VPS and a Host behind NAT, installed with `install.sh`, a real stack imported and taken over, a week of normal use.
It also covers what v0.1 never tested on a real server (Q97): a fresh install following the README only, the VPS's
own Host Key and its secrets, `omegacd import` of the stacks already running there and their takeover, a private Git
repository over SSH, and images from a private registry.

The scenarios, with steps and expected results, are TESTING.md's "Real-world run (v0.2, M7-04)" checklist: each ticket
that builds one of its parts adds its exact commands.

**Done when:** every row of that checklist is run and recorded in TESTING.md with what was found and fixed.

### M7-05 · Documentation
**Status:** todo
**Depends on:** M7-03

README rewritten for v0.2 (concepts, install modes, Agents, Source Server, proxies, security model, upgrade note:
reinstall from v0.1), TESTING.md, ROADMAP.md, SECURITY.md (trust boundaries), ADRs final, screenshots. The Agent's
support table (Q61: Agents on Debian/Ubuntu, amd64 and arm64; the RHEL family after v0.2; macOS for development only), the
work dir in RAM vs on disk (where decrypted secrets live, what a reboot does, Q19/Q59) and the file modes of decrypted
secrets per engine (Q58) get their own section, from the draft [user-ids.md](../user-ids.md): rootful vs rootless,
who owns which files, data folders and `chown`, the "user 1000" convention, choosing an engine.

**Done when:** a new user installs from the README alone (the v0.1 checklist's test 6, redone).

### M7-06 · RHEL-family test matrix
**Status:** deferred: after v0.2 (Q61, 2026-11-08; ROADMAP)
**Depends on:** M6-07

`test/linux.sh` gets Rocky and Fedora machines next to Ubuntu, arm64 and amd64, with the same tests and engines, and a
Rocky VM with SELinux enforcing (OrbStack's kernel has no SELinux policy; a Lima VM locally, KVM in CI). A first
version was built during M2 and removed when v0.2 narrowed to Debian/Ubuntu (kept only on the maintainer's local branch
`archive/rhel-test-machines`). What it taught:
- Docker CE comes from Docker's dnf repository; `age` isn't packaged (not even in EPEL), so it and `sops` come from
  their releases, and checks for them go by path: sudo's `secure_path` on RHEL lacks `/usr/local/bin`.
- SELinux: Docker needs `"selinux-enabled": true`; the VM's own rootless Podman confines containers by default. Tests
  that mount a user's own folder need `:z` like a real stack would; containers that mount the Docker socket need
  `label=disable` (never relabel the socket).
- Emulated amd64 (Rosetta) Rocky had its Docker daemon hang twice in a day: prefer native machines or CI runners for
  amd64.

**Done when:** `make test-linux` is green on Ubuntu, Rocky and Fedora on both architectures, and the SELinux
enforcing suite is green on a RHEL-family VM.

### M7-07 · Forgejo becomes primary
**Status:** todo
**Depends on:** M7-05

The project moves from GitHub to the maintainer's Forgejo, where CI runs on the maintainer's own runners. Until then Forgejo is a
pull mirror of GitHub (every 10 minutes, Actions off). The switch:
- **Migrate once** with Forgejo's GitHub migration: issues, PRs, comments, labels, milestones, releases; the mirror
  is replaced.
- **CI on Forgejo Actions** with the maintainer's runners, labelled by need: `ubuntu-24.04-amd64`,
  `ubuntu-24.04-arm64`, `debian-13-amd64`, `debian-13-arm64` (host jobs, one at a time: the gate), and `docker` for
  light container jobs (web UI, secret scan); jobs from others' pull requests only after approval.
- **Releases stay public** while the source may become private:
  - images on Docker Hub (`obleey/omegacd`), optionally a second registry as a fallback;
  - binaries and `install.sh` from a public place, chosen then (a release-only repository, an own domain with object
    storage, or the Forgejo exposed);
  - the README, the install docs and the `readme-walkthrough` e2e follow.
- **Tooling moves over:** `create-issues.sh`, CLAUDE.md's commands and the release workflow move from `gh` to Forgejo.
- **GitHub goes private** only after the public download places exist.

**Done when:** an issue, a pull request with its CI on the maintainer's runners, and a release (image pulled from Docker
Hub, binary and install script downloaded by someone not logged in) all work on Forgejo alone.
