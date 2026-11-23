# M6 · Install and import

Getting a Control Plane and Agents running anywhere, and moving existing stacks in.
See [architecture](../architecture.md#install-q10-q21-q26-q33-q35-q51).

Order (Q229): M6-02, M6-03, M6-09, M6-01, M6-04 (deferred, Q276), M6-06, M6-08, M6-11 (Q299), M6-05, then M6-10.

### M6-01 · install.sh: full and agent modes
**Status:** done (2026-11-22, PR #158)
**Depends on:** M4-04, M3-02

`install.sh` asks **full** (Control Plane, UI, Source Server) or **agent** (Control Plane URL + invite; container or
systemd; rootless detected) (Q33). An Agent on the Control Plane's machine is installed in agent mode and enrolled by
hand like any other (Q104). Unattended via environment variables,
as today; Kubernetes context always confirmed.

On Debian it installs with recommended packages, as apt does by default: a minimal image without them (Hetzner's
turns them off) lacks the docker command (`docker-cli`), Podman's networking (`passt`) and its DNS (`aardvark-dns`),
found by the test servers; the docs say so.

Its agent-mode one-liner is a tab of the UI's Add Host dialog, with the invite filled in (M5-01, Q151).

Agent mode installs the `.deb` (M6-09) where apt is there, the system unit with a docker-group user or, on a rootless
engine, the package's user unit with lingering; the container (`deploy/agent.yaml`) when asked or without apt (Q232).
Full mode keeps compose and kubernetes (kubernetes dropped for v0.2, Q276), and drops v0.1's `age-keygen` step, `known_hosts` file and "Master Key for
.sops.yaml" line; it ends with "open the UI and add a Host" (Q233). It installs the version it was released with: the
release workflow attaches it to each GitHub Release with the version filled in, and tests point it at local builds
(Q234).

From M6-09 (Q255): the package makes `omegacd-agent`, so agent mode writes `/etc/omegacd/agent.env` (0640,
`root:omegacd-agent`) after installing it, then starts the Agent (the package starts it only if the file was there); on
a rootless engine it enables the package's user units (`systemctl --user enable`, with lingering) and on Podman copies
`/usr/share/omegacd/containers.conf.d/omegacd-agent.conf` into the user's `~/.config/containers/containers.conf.d/`.

**Done when:** both modes are tested on a clean machine, rootful and rootless.

Settled before it (Q262–Q269): kubernetes keeps its key step until M6-04; the settings (`OMEGACD_MODE`, the Agent's own
names, `OMEGACD_AGENT_INSTALL`, `OMEGACD_AGENT_ENGINE` in Q103's order); run as the engine's user, `sudo` only where
needed; agent mode's system unit, user units with lingering (Podman: drop-in, `podman-restart`) or container (Q240's
rootless values, Q252's refusal); the release attaches it with its version, the `.deb` checked against `SHA256SUMS`,
`main`'s copy refuses; a rerun upgrades; Add Host's install.sh tab first; `systemd.sh` and `deploy-compose.sh` install
through it, so the gate's real reboot covers what it set up. The test servers aren't fresh machines: each test purges
what it installs first.

### M6-02 · Compose files
**Status:** done (2026-11-20, PR #154)
**Depends on:** M4-04

`deploy/compose.yaml` (full: Control Plane + a standalone Source Server service by default, its mTLS certificates made
at first start by a one-shot `omegacd source-certs` into a shared volume, Q231) and `deploy/agent.yaml`
(socket, state volume, work dir at the same path both sides, rootless variant with the user socket) (Q21). The image
runs as a non-root user; only the Agent's service gets the Docker socket, in its group (`group_add`); the Control
Plane's gets none (Q104).

From M4-06 (Q115): the Control Plane creates its Master Key when missing, but `deploy/compose.yaml` mounts `age.key` as
a single read-only file, which must exist first. Mount a key folder instead (writable), so a compose install needs no
`age-keygen` step either.

`deploy/agent.yaml` is a tab of the UI's Add Host dialog, with the invite filled in (M5-01, Q151).

Settled before the ticket (Q235–Q240): the Agent finds its container by `/proc/self/mountinfo`; the image's user is
`omegacd` (10001); `deploy/agent.yaml` uses host networking and `group_add` from a `.env`, its rootless variant runs as
container root (the user on the host), which the Agent accepts only in a container on a rootless engine; a work dir it
can't write is given to it through Docker at start; e2e `deploy-compose.sh`.

Found running the Agent in a container (2026-11-18): it finds its own container by its hostname to check the work
dir's path parity, so with host networking (which lets it reach a Control Plane on the same machine over
`http://127.0.0.1`) it can't, and stops. Find the container without the hostname (its id in `/proc/self/mountinfo`
or the cgroup path), or set `hostname:` to `container_name:` in `deploy/agent.yaml`.

**Done when:** `docker compose up -d` brings each up, as a non-root user; the Agent refuses a mount without path parity.

### M6-03 · systemd units
**Status:** done (2026-11-21, PR #156)
**Depends on:** M2-01

A user unit for rootless (`~/.config/systemd/user/omegacd-agent.service`, lingering enabled) and a system unit with a
dedicated docker-group user (Q10). With the system unit, `omegacd-agent-restore.service` (oneshot, as that user,
`omegacd agent restore --files`) is ordered `Before=docker.service`, so the files are back in the RAM work dir before
Docker restarts any container (Q59).

For a Podman Host the Agent's user gets `events_logger = "file"` (Self-Heal reads events from it) and
`log_driver = "k8s-file"` (Podman can't follow journald logs with events in a file) in its `containers.conf.d`, with
`log_size_max` set (k8s-file logs grow without limit otherwise; journald rotated them).

No tab of its own in Add Host (Q247): M6-01's `install.sh` tab installs these units with the `.deb`; until then the
README has the manual steps.

Settled before it (Q242–Q251): the units in `packaging/` (`omegacd-agent` user in the docker group,
`/etc/omegacd/agent.env`, `/usr/bin/omegacd`); the work dir a preserved `RuntimeDirectory`; a restore unit in the
user's scope too, before rootless Docker and `podman-restart.service`; hardening without `PrivateTmp`/`ProtectHome`
(path parity); Podman on the user unit with a `containers.conf.d` drop-in (`log_size_max` 10 MiB); an e2e simulating
the reboot (system unit on rootful Docker, user unit on rootless Docker and Podman) and a real reboot in the gate; the
Control Plane unit's v0.1 leftovers dropped.
Found while building it (Q252): on rootless Docker the container Agent reaches its Control Plane by its `https://` URL
only, as RootlessKit's host network has its own loopback; tested through a TLS front.

From M6-02 (Q241): native rootless Docker for the test user on the test servers (omegacd-testinfra's Ansible), which
the rootless user unit's test needs, and with it `deploy-compose.sh`'s rootless Docker variant of `deploy/agent.yaml`
(M6-02 proves the rootless variant on Podman only).

**Done when:** both survive a reboot with apps restored offline, including services with a restart policy on rootful
Docker.

### M6-04 · Kubernetes
**Status:** deferred: after v0.2 (Q276, 2026-11-22; ROADMAP)
**Depends on:** M1-05

`deploy/kubernetes.yaml`: Control Plane + Source Server (separate pods, mTLS between), no Agent (Q33, Q104), and
`install.sh`'s `kubernetes` target for it.

Deferred (Q276): v0.2 is for Docker hosts, and dropped v0.1's manifest, the `kubernetes` target and the README's
Kubernetes option. Designed before the deferral (Q270–Q275) and built on the branch `park/m6-04-kubernetes`: the
manifest, the target making the certificates Secret in a pod of the image, and `test/e2e/kubernetes.sh`, passing on
OrbStack; still to do there: the run in kind on the Linux servers and CI, the README section and TESTING.md.

**Done when:** installs in OrbStack's Kubernetes and in kind on every Linux target, and manages a remote Agent.

### M6-05 · import through the Agent
**Status:** todo
**Depends on:** M4-01

`omegacd import --host NAME` asks that Host's Agent to import its running Compose stacks: the Agent runs the importer
and returns the finished files, every secret already encrypted, so the Control Plane relays no plaintext secret; the
CLI sends the recipients and the names already in the checkout, and writes the files (Q230). Same report and files as today; the SSH path is removed (Q47). Fix the `../` handling
from issue #9 here (copy referenced files into the app, refuse paths inside other tools' storage).

The import encrypts for the Host's key plus the people's keys allowed for it, and refuses a Host-only result without
`--host-only` (Q194, Q201; the key list is M6-08's, done before this ticket, Q229).

**Done when:** today's importer tests pass through a fake Agent, and an integration test imports from a real Agent.

### M6-06 · Reverse proxy documentation
**Status:** done (2026-11-22, PR #160)
**Depends on:** M3-01

Examples for Caddy, Traefik and nginx in front of the Control Plane (WebSocket upgrade, timeouts, trusted proxies),
and the TLS-passthrough note for a proxy in front of a standalone Source Server (Q51).

Settled before it (Q277–Q285): Compose overrides in `deploy/proxy/` tested as shipped; trust only the proxy's fixed
address on a network of the two; Cloudflare in front works unchanged, a README paragraph and a hand check in M7-04.

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
**Status:** done (2026-11-22, PR #162)
**Depends on:** M3-02, M2-05

`omegacd secrets init --host NAME [--path PATH] [--source sops]` prepares a repository for a Host's secrets, so
nobody copies `age1…` strings by hand: for SOPS it writes or updates `.sops.yaml` with a rule for the path, the
Host's public key (from the Control Plane) and the user's own recipient (from `omegacd keygen`, the one that keeps
files editable and recoverable when a Host is lost), and `omegacd secrets rekey` runs `sops updatekeys` after a Host
changed. Built per secret source (Q29): later sources (KeePass, 1Password, Ansible Vault, OpenBao, …) bring their own
`init` (a reference format, not keys); `import` uses the same code.

Editing secrets (Q189–Q201): Settings → Encryption keys (people's public age keys, each for all Hosts or chosen ones;
`omegacd secrets key add`; readable by any login, changed by the admin), and every rule OmegaCD writes or copies gets
the Host's key plus the keys allowed for it (the Host card, `secrets init` with one rule per Application path, the
import). `secrets init` and `import` refuse a Host-only rule without `--host-only`; the Host card warns. `secrets
rekey` rekeys every file it can and lists the ones it can't open. An Application warns when its files are encrypted
for an outdated set of keys. The Master Key is never a recipient. `omegacd keygen` writes a person's key to
where `sops` reads it by default (Q290).  The docs say the key list governs reading, not changing (that's Git's
branch protection), and to change secret values when someone leaves.

Settled before it (Q286–Q297): rules per Application from the checkout's origin, marked `# omegacd: Host NAME` so a
rekey refreshes them; rekey with the SOPS library, no `sops` binary; the warning compared on the Source Server from
the recipients the Control Plane sends; `keygen` where `sops` reads keys. A Host with Applications gets a new Host Key
through `app delete --forget`, `host delete` and `host add` (Q127, Q289).

**Done when:** a repository prepared with it deploys a SOPS secret to that Host, and the people's keys allowed for it open
and edit it; a Host-only rule is refused without `--host-only`; a file encrypted for an outdated set of keys shows its
warning; rekeying after a Host Key rotation
keeps it deployable and editable with the user's key; tested end to end; and the helper's interface has a second
(dummy) source.

### M6-09 · .deb packages
**Status:** done (2026-11-22, PR #157)
**Depends on:** M6-03

Every release builds `.deb` packages for amd64 and arm64 (nfpm, in the release workflow) and attaches them to the
GitHub Release, next to the binaries. The package holds the binary and the systemd units (M6-03, the files restore
before Docker included); its scripts create the Agent's user and folders and enable the units, and remove them
cleanly. `install.sh` (M6-01) installs the package where apt is there, so there is one install path. `.rpm` comes
with the RHEL family and a signed apt/dnf repository (`apt upgrade`) later, both after v0.2 (ROADMAP).

**Done when:** on the test servers (Ubuntu 24.04 and Debian 13, amd64 and arm64) the package installs, the Agent runs and survives a
reboot with its apps restored, an upgrade to a newer package keeps the state and the apps running, and uninstalling
leaves no unit or folder but the state the admin chose to keep; the system users stay, as Debian's packages leave
them, so nothing they own is left to a reused uid (Q261).

Settled before it (Q253–Q260): one `omegacd` package (nfpm through `go run`, `make deb`) with the Control Plane's unit
too, never enabled by it; `/etc/omegacd` root's, the Master Key in `/etc/omegacd/keys/`; users from `sysusers.d`, the
Agent's system units enabled with `ConditionPathExists=/etc/omegacd/agent.env`, the user units and Podman's drop-in
shipped, never enabled; an upgrade restarts the running Agents, the lingering users' too; remove keeps the state, purge
removes the Agent's, never the Control Plane's data or Master Key, nor the users (Q261); assets named like the archives;
`systemd.sh` tests the package, and the gate's real reboot with it.
Found while building it (Q261): purge deleting the users would leave the kept Master Key, and any file the Agent wrote
outside its folders, to whichever user gets the uid next, and could delete a user the package didn't make (the test
servers' own login is `omegacd`, which `sysusers` keeps); so the users stay.

### M6-10 · Decrypted files as bind mounts
**Status:** todo
**Depends on:** M4-01

A Sync refuses a relative bind mount whose source isn't in Git (`checkBindSources`, so data written there isn't lost
on the next Sync). A decrypted file is never in Git, only its `.sops.` original: `./app.yaml:/etc/app.yaml:ro` from
`app.sops.yaml` fails "mounts ./app.yaml, which is not in Git", though a Compose secret (`secrets: … file: ./app.yaml`)
or an `env_file` of the same file passes. Accept a bind source whose `.sops.` original is in Git, as well as a plain
file in Git, which mounts today and must go on doing so; anything else not in Git stays refused. The README's Secrets section shows the mount next to `env_file` and `secrets:`.

Found while building M6-03, whose `systemd.sh` mounts its decrypted file as a Compose secret instead.

**Done when:** an app mounting both a plain file from Git and a decrypted file as bind volumes deploys and its container
reads both, they come back after `systemd.sh`'s simulated reboot, and a mount of a file neither in Git nor decrypted
from one is still refused with the same message.

### M6-11 · Initial admin password, changed at first login
**Status:** done (2026-11-23, PR #167)
**Depends on:** M6-01, M6-08

The admin's first password is always generated, never configured (Q298): `OMEGACD_ADMIN_PASSWORD` and all its code go
(Q301; an install still setting it simply has it ignored, an existing admin keeps their password). At first start the
Control Plane writes the generated password once to `initial-admin-password` in its data dir (mode 0600) and logs
only where it is, never the password: the logging exception of Q46 goes (Q300). The first login must change it, the
API refusing everything else until then (as today for a generated one); the change deletes the file. `install.sh full`
prints it at the end; by hand `docker compose exec omegacd cat /data/initial-admin-password`. One shared e2e helper
reads the file, logs in, changes the password and makes the API token; every e2e script, `deploy-compose.sh` and the
README walkthrough use it (Q302).

Settled before it (Q311–Q316): `omegacd reset-admin-password` on the Control Plane's machine writes a new one into
the file and requires it changed (the `.deb`: `sudo omegacd-reset-admin-password`, as the service's user); every start logs the file's path while it exists; never in a backup; the login page
says where the first password is.

**Done when:** a fresh install has no admin password setting; the initial password is only in that file, never in a
log; the first login must change it and the change removes the file; `install.sh full` shows it; README, TESTING.md
and the e2e suite use the file; tested end to end.
