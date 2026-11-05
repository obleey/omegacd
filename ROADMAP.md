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

## v0.2 — trustworthy for real sites

- **Sync hooks** — jobs before/after a Sync: `artisan migrate`, `cache:clear`, `wp core update-db`, backups before upgrades
- **Order between Child Applications** — `dependsOn: [caddy]` in a definition: the App of Apps syncs children in that order (e.g. the app that creates a shared network before those that join it), instead of a failed first attempt and a retry
- **Notifications** — Slack, Discord, email, generic webhook on Sync failed / Degraded / recovered
- **Restart unhealthy containers** — Self-Heal option `restartUnhealthy`: restart a container its own healthcheck reports unhealthy for a while (Docker's restart policies only react to exits), with backoff and a limit, each restart in History and notified
- **Kubernetes polish** — on startup, check remote Hosts' files on the Host itself (today a pod without its work dir volume would restore and recreate every Application)
- "Sync and recreate" in the UI; refresh from Git when an Application page opens

## v0.3 — many Applications, little repetition

- **ApplicationSet** — one template + generators (list, Git directories, Hosts) generate Applications
- **Image updater** — watch registries for new tags and commit the bump to Git
- **Follow the latest Git tag** — `targetRevision: "v1.*"` or a semver range (`>=1.4 <2.0`): deploy the newest matching tag automatically, instead of editing the Application for each release
- **Podman support** — run against Podman's Docker-compatible API socket (rootful and rootless): enable/check `podman-restart.service` so containers come back after a reboot, an SSH transport that doesn't need the Docker CLI on the server, rootless notes (ports < 1024, UID mapping on bind mounts), and the integration test suite running against Podman in CI; the engine (Docker or Podman, with version) detected per Host and shown as a badge on the Host and on every Application deployed there, so mixed fleets are obvious. Quick test against Podman 5.8 (Docker-compatible API): deploy, SOPS, bind-mounted files, diff, logs, events/drift and delete-with-volumes work; healthchecks need systemd on the Podman host (document it)
- Nested App of Apps
- Name prefix per App of Apps, so two of them can declare an Application with the same name (e.g. `edge-caddy`, `blog-caddy`)
- Theme picker (all Catppuccin flavors)

## v0.4 — teams

- **SSO** via OpenID Connect (Google, GitHub, GitLab, Microsoft Entra, Keycloak, Authentik, …), multiple users, RBAC (e.g. access per customer / App of Apps)
- Audit log
- **External secret managers** as Secret Sources next to SOPS: HashiCorp Vault / OpenBao, 1Password, Bitwarden, Infisical, AWS Secrets Manager, Google Secret Manager, Azure Key Vault — referenced from compose, resolved at Sync time, never stored in Git
- Postgres as an alternative to SQLite

## Later

- Docker Swarm
- Agent mode for Hosts behind NAT or firewalls
- Agent mode: Host Keys that never leave the Host (decrypt on the Host)
- Web terminal into containers
- OmegaCD upgrading itself

## Related projects

- **SOPS web GUI** (separate project, planned) — encrypt secrets in the browser and commit them, without the `sops` CLI; like kubeseal-webgui for Sealed Secrets. OmegaCD could embed it as an "Encrypt secret" page.

## Housekeeping

- Start watching a newly added Host's events immediately (today: within 30 s)
- [x] Renovate for Go, npm, GitHub Actions and container images
- [x] Gzip for the web UI
- Copy only changed files to remote Hosts
- A documentation site
