# Roadmap

OmegaCD is early. This is the plan, most important first — it changes as real users report what they need. Want something? [Open an issue](https://github.com/obleey/omegacd/issues) or upvote an existing one.

## v0.1 — first public release

- [x] Applications from Git: Compose files, profiles, diff, Sync, Auto-Sync, Self-Heal, Prune
- [x] Sync and Health Status, live via Docker events; history and Rollback
- [x] SOPS + age secrets, decrypted only at Sync time; one Host Key per Host
- [x] Remote Hosts over SSH or TLS
- [x] App of Apps (Applications declared in Git); Refresh cascades to its children
- [x] Web UI, REST API, CLI (`omegacd app sync --wait` for CI), Prometheus metrics
- [x] Restore working files after a reboot; a persistent work dir (e.g. `/opt/services`) as an option
- [x] Repository access check in the Application form: public / private-with-credential / access denied while you type, with an inline "Add credential"
- [x] Diff tab with Live (compose files on the Host), Desired (in Git) and a Compose diff; syntax colors for ~240 config formats
- [x] Relative bind mounts not in Git are refused (every Sync would delete their data); warning for secrets written in plain text in compose files
- [x] Work dir holds only what containers read (no `*.sops.*` copies)
- [x] Webhooks (GitHub, GitLab, Gitea/Forgejo); polling as the safety net, `OMEGACD_POLL_INTERVAL=0` for webhooks only
- [x] The reason an Application is Unknown shown on its tile (e.g. a Git token that stopped working)
- [x] Install script for Docker Compose and Kubernetes (`deploy/install.sh`), `deploy/compose.yaml`, `deploy/kubernetes.yaml`, `omegacd keygen`
- [x] Kubernetes: `OMEGACD_LOCAL_HOST=false` hides the `local` Host where no Docker engine runs
- [x] `omegacd import`: move stacks started by hand into Git from your laptop over SSH — config into Git, data stays, secrets encrypted for the Host Key, `adopt: true` definitions
- [x] Release candidates `v0.1.0-rc.1` … `rc.4`: binaries and multi-arch image published by CI

Still to test before v0.1.0 (see [TESTING.md](TESTING.md)):

- [ ] Fresh install on a clean VPS with the install script, following the README only
- [ ] That VPS as a remote Host (SSH): deploy, files, health, logs, its own Host Key
- [ ] `omegacd import` of the stacks already running on that VPS, and their takeover
- [ ] A private Git repository over SSH (token and webhooks are tested)
- [ ] Images from a private registry

## v0.2 — pull-based Agents (in development on `main`)

A breaking redesign, and the MVP: what a small team needs to run its Compose Hosts with it, on Debian/Ubuntu; more
platforms (the RHEL family) and features come after it. v0.1 continues on the `release-0.1` branch for fixes only,
and both are released together.
The plan: [docs/v0.2](docs/v0.2/README.md) (architecture, decisions, 8 milestones of tickets).

- **Agents** on every Host dial out to the Control Plane over one WebSocket and run Compose themselves; push over SSH/TLS goes away
- **Secrets decrypted only on the Agent**, with a Host Key born there; secret sources plug in on the Agent
- **Source Server**: a separate component owning Git credentials and rendering; in-process by default, standalone with mTLS for production
- **No data loss**: Agents keep, restore and self-heal their apps with the Control Plane or Source Server gone; offline rollback on the Host
- **Enrollment** with single-use invites, Agent key pairs, fingerprint approval and signed challenges
- **Rootless** Docker, docker-group users and rootless **Podman** (tested); Agent as a container or a systemd service
- Built-in Agent in a full install; install script with full and agent modes; Kubernetes; reverse proxies everywhere
- UI: Add Host flow, Host cards, engine badges everywhere, offline and queued states; then a polish milestone
- **Hosts**: Debian 13 and Ubuntu 24.04 on amd64 and arm64, every push tested on all four with the same tests (`make test-linux`, real servers); macOS for development only
- **Packages**: `.deb` for amd64 and arm64 on every release, with the systemd units; `install.sh` installs it (`.rpm` comes with the RHEL family, after v0.2)
- **Logging everywhere**: error, warn, info and debug for every decision the code makes, one Sync followable across Control Plane, Source Server and Agent, json or text; never a secret in a log
- **Secrets helpers**: `omegacd secrets init` writes `.sops.yaml` for a Host, and a clear "this Host can't decrypt it" when a file was encrypted for other keys; built per secret source, ready for KeePass, 1Password, Ansible Vault, OpenBao and others later

## After v0.2 — trustworthy for real sites (to be re-planned)

- **Ubuntu 26.04** — the current LTS (newer Docker, Podman, systemd) as a supported Host, tested like 24.04: two more test servers
- **RHEL-family Hosts** — RHEL, Rocky, Alma, Fedora with SELinux enforcing, Podman or Docker CE: install, systemd units with SELinux contexts, `.rpm`, and the test matrix in every push. The test machines for it (Rocky, Fedora, a Rocky VM with SELinux enforcing) were built and removed again, what they taught is in M7-06; v0.2's tickets M6-07 and M7-06 move here

- **Sync hooks** — jobs once per Sync, before/after it: backups before upgrades, `cache:clear`, `wp core update-db` (migrations on every start already work with Compose's own one-shot services, tested in v0.2's M2-11). How they fit the Agent and the protocol is decided before v0.2's M3 ([open questions](docs/v0.2/decisions.md#open-for-the-grilling-before-m3))
- **Order between Child Applications** — `dependsOn: [caddy]` in a definition: the App of Apps syncs children in that order (e.g. the app that creates a shared network before those that join it), instead of a failed first attempt and a retry
- **apt repository** (dnf with the RHEL family) — a signed repository for the packages, so `apt upgrade` updates OmegaCD (the signing key lives in CI secrets)
- **Notifications** — Slack, Discord, email, generic webhook on Sync failed / Degraded / recovered
- **Restart unhealthy containers** — Self-Heal option `restartUnhealthy`: restart a container its own healthcheck reports unhealthy for a while (Docker's restart policies only react to exits), with backoff and a limit, each restart in History and notified
- **Find apps by Host and App of Apps** — filters for Host and App of Apps next to Sync and Health (with counts); every Host is clickable into its apps (a Host card's app count, the Host badge on an app); filters kept in the URL (back, reload, shared links); group by App of Apps or by Host, each group with a health summary; maybe search tokens (`host:prod-1`). Written up for a grilling: [docs/ideas/find-apps.md](docs/ideas/find-apps.md)
- "Sync and recreate" in the UI; refresh from Git when an Application page opens

## v0.3 — many Applications, little repetition

- **Web terminal** into containers through the Agent (paid)
- **GitHub SSO** (paid)
- **ApplicationSet** — one template + generators (list, Git directories, Hosts) generate Applications
- **Image updater** — watch registries for new tags and commit the bump to Git
- **Follow the latest Git tag** — `targetRevision: "v1.*"` or a semver range (`>=1.4 <2.0`): deploy the newest matching tag automatically, instead of editing the Application for each release
- **Podman support** beyond v0.2's tested rootless setup — run against Podman's Docker-compatible API socket (rootful and rootless): enable/check `podman-restart.service` so containers come back after a reboot, an SSH transport that doesn't need the Docker CLI on the server, rootless notes (ports < 1024, UID mapping on bind mounts), and the integration test suite running against Podman in CI; the engine (Docker or Podman, with version) detected per Host and shown as a badge on the Host and on every Application deployed there, so mixed fleets are obvious. Quick test against Podman 5.8 (Docker-compatible API): deploy, SOPS, bind-mounted files, diff, logs, events/drift and delete-with-volumes work; healthchecks need systemd on the Podman host (document it)
- Nested App of Apps
- Name prefix per App of Apps, so two of them can declare an Application with the same name (e.g. `edge-caddy`, `blog-caddy`)
- Theme picker (all Catppuccin flavors)

## v0.4 — teams

- **SSO** via OpenID Connect (Google, GitHub, GitLab, Microsoft Entra, Keycloak, Authentik, …), multiple users, RBAC (e.g. access per customer / App of Apps)
- Audit log
- **External secret managers** as Secret Sources next to SOPS, resolved on the Agent (paid): HashiCorp Vault / OpenBao, 1Password, Bitwarden, Infisical, AWS Secrets Manager, Google Secret Manager, Azure Key Vault — referenced from compose, resolved at Sync time, never stored in Git
- Postgres as an alternative to SQLite

## Later

- Docker Swarm
- OmegaCD upgrading itself

## Related projects

- **SOPS web GUI** (separate project, planned) — encrypt secrets in the browser and commit them, without the `sops` CLI; like kubeseal-webgui for Sealed Secrets. OmegaCD could embed it as an "Encrypt secret" page.

## Housekeeping

- [x] Renovate for Go, npm, GitHub Actions and container images
- [x] Gzip for the web UI
- A documentation site
