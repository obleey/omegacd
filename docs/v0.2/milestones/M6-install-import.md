# M6 · Install and import

Getting a Control Plane and Agents running anywhere, and moving existing stacks in.
See [architecture](../architecture.md#install-q10-q21-q26-q33-q35-q51).

### M6-01 · install.sh: full and agent modes
**Status:** todo
**Depends on:** M4-04, M3-02

`install.sh` asks **full** (Control Plane, UI, Source Server, built-in Agent when Docker is there) or **agent**
(Control Plane URL + invite; container or systemd; rootless detected) (Q33). Unattended via environment variables,
as today; Kubernetes context always confirmed.

**Done when:** both modes are tested on a clean machine, rootful and rootless.

### M6-02 · Compose files
**Status:** todo
**Depends on:** M4-04

`deploy/compose.yaml` (full: Control Plane + optional standalone Source Server service) and `deploy/agent.yaml`
(socket, state volume, work dir at the same path both sides, rootless variant with the user socket) (Q21).

**Done when:** `docker compose up -d` brings each up; the Agent refuses a mount without path parity.

### M6-03 · systemd units
**Status:** todo
**Depends on:** M2-01

A user unit for rootless (`~/.config/systemd/user/omegacd-agent.service`, lingering enabled) and a system unit with a
dedicated docker-group user (Q10).

**Done when:** both survive a reboot with apps restored offline.

### M6-04 · Kubernetes
**Status:** todo
**Depends on:** M1-05

`deploy/kubernetes.yaml`: Control Plane + Source Server (separate pods, mTLS between), no built-in Agent (Q33, Q35).

**Done when:** installs in OrbStack's Kubernetes and manages a remote Agent.

### M6-05 · import through the Agent
**Status:** todo
**Depends on:** M4-01

`omegacd import --host NAME` asks that Host's Agent to read its running Compose stacks; the Host Key's public key
comes from the Control Plane. Same report and files as today; the SSH path is removed (Q47). Fix the `../` handling
from issue #9 here (copy referenced files into the app, refuse paths inside other tools' storage).

**Done when:** today's importer tests pass through a fake Agent, and an integration test imports from a real Agent.

### M6-06 · Reverse proxy documentation
**Status:** todo
**Depends on:** M3-01

Examples for Caddy, Traefik and nginx in front of the Control Plane (WebSocket upgrade, timeouts, trusted proxies),
and the TLS-passthrough note for a proxy in front of a standalone Source Server (Q51).

**Done when:** each example is tested in the e2e suite or by hand and recorded in TESTING.md.
