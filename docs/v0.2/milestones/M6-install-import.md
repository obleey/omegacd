# M6 · Install and import

Getting a Control Plane and Agents running anywhere, and moving existing stacks in.
See [architecture](../architecture.md#install-q10-q21-q26-q33-q35-q51).

### M6-01 · install.sh: full and agent modes
**Status:** todo
**Depends on:** M4-04, M3-02

`install.sh` asks **full** (Control Plane, UI, Source Server, built-in Agent when Docker is there) or **agent**
(Control Plane URL + invite; container or systemd; rootless detected) (Q33). Unattended via environment variables,
as today; Kubernetes context always confirmed.

On Debian it installs with recommended packages, as apt does by default: a minimal image without them (Hetzner's
turns them off) lacks the docker command (`docker-cli`), Podman's networking (`passt`) and its DNS (`aardvark-dns`),
found by the test servers; the docs say so.

**Done when:** both modes are tested on a clean machine, rootful and rootless.

### M6-02 · Compose files
**Status:** todo
**Depends on:** M4-04

`deploy/compose.yaml` (full: Control Plane + optional standalone Source Server service) and `deploy/agent.yaml`
(socket, state volume, work dir at the same path both sides, rootless variant with the user socket) (Q21). The image
runs as a non-root user in the Docker socket's group (`group_add`), not root (Q89).

**Done when:** `docker compose up -d` brings each up, as a non-root user; the Agent refuses a mount without path parity.

### M6-03 · systemd units
**Status:** todo
**Depends on:** M2-01

A user unit for rootless (`~/.config/systemd/user/omegacd-agent.service`, lingering enabled) and a system unit with a
dedicated docker-group user (Q10). With the system unit, `omegacd-agent-restore.service` (oneshot, as that user,
`omegacd agent restore --files`) is ordered `Before=docker.service`, so the files are back in the RAM work dir before
Docker restarts any container (Q59).

For a Podman Host the Agent's user gets `events_logger = "file"` (Self-Heal reads events from it) and
`log_driver = "k8s-file"` (Podman can't follow journald logs with events in a file) in its `containers.conf.d`.

**Done when:** both survive a reboot with apps restored offline, including services with a restart policy on rootful
Docker.

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

### M6-07 · RHEL-family Hosts
**Status:** deferred: after v0.2 (Q61, 2026-11-08; ROADMAP)
**Depends on:** M6-01, M6-03

Install and run on RHEL, Rocky, Alma (9 and 10) and Fedora (Q60): `install.sh` with dnf, Podman (rootless, the
default there) or Docker CE, the systemd units (M6-03) with correct SELinux contexts, a firewalld note for the Control
Plane's port. SELinux stays **enforcing**: the Agent adds `:z` to app-folder bind mounts, its helpers and the
containerised Agent (the Docker socket) run with `label=disable` (Q60, M7-06's notes), and the units get their
contexts, so the restore unit (Q59) works under SELinux too.

**Done when:** on an enforcing RHEL-family VM, an Application with bind-mounted files and a decrypted secret deploys,
survives a reboot offline and self-heals, with no SELinux denial in the audit log (`ausearch -m avc`), for rootless
Podman and for Docker CE.

### M6-08 · Secrets helper for a repository
**Status:** todo
**Depends on:** M3-02, M2-05

`omegacd secrets init --host NAME [--path PATH] [--source sops]` prepares a repository for a Host's secrets, so
nobody copies `age1…` strings by hand: for SOPS it writes or updates `.sops.yaml` with a rule for the path, the
Host's public key (from the Control Plane) and the user's own recipient (from `omegacd keygen`, the one that keeps
files editable and recoverable when a Host is lost), and `omegacd secrets rekey` runs `sops updatekeys` after a Host
changed. Built per secret source (Q29): later sources (KeePass, 1Password, Ansible Vault, OpenBao, …) bring their own
`init` (a reference format, not keys); `import` uses the same code.

**Done when:** a repository prepared with it deploys a SOPS secret to that Host; rekeying after a Host Key rotation
keeps it deployable and editable with the user's key; tested end to end; and the helper's interface has a second
(dummy) source.

### M6-09 · .deb packages
**Status:** todo
**Depends on:** M6-03

Every release builds `.deb` packages for amd64 and arm64 (nfpm, in the release workflow) and attaches them to the
GitHub Release, next to the binaries. The package holds the binary and the systemd units (M6-03, the files restore
before Docker included); its scripts create the Agent's user and folders and enable the units, and remove them
cleanly. `install.sh` (M6-01) installs the package where apt is there, so there is one install path. `.rpm` comes
with the RHEL family and a signed apt/dnf repository (`apt upgrade`) later, both after v0.2 (ROADMAP).

**Done when:** on the test servers (Ubuntu 24.04 and Debian 13, amd64 and arm64) the package installs, the Agent runs and survives a
reboot with its apps restored, an upgrade to a newer package keeps the state and the apps running, and uninstalling
leaves no unit, user or folder but the state the admin chose to keep.
