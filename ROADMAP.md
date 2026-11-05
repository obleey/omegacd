# Roadmap

OmegaCD is early. This is the plan, most important first — it changes as real users report what they need. Want something? [Open an issue](https://github.com/obleey/omegacd/issues) or upvote an existing one.

## v0.1 — first public release

- [x] Applications from Git: Compose files, profiles, diff, Sync, Auto-Sync, Self-Heal, Prune
- [x] Sync and Health Status, live via Docker events; history and Rollback
- [x] SOPS + age secrets, decrypted only at Sync time
- [x] Remote Hosts over SSH or TLS
- [x] App of Apps (Applications declared in Git)
- [x] Web UI, REST API, CLI (`omegacd app sync --wait` for CI), Prometheus metrics
- [x] Restore working files after a reboot
- [ ] Real-world test: a full arr stack as an App of Apps on a Hetzner VPS
- [ ] Real-world test: private Git repos (token and SSH), webhooks from GitHub, private registry images
- [x] Repository access check in the Application form: shows public / private-with-credential / access denied while you type, with an inline "Add credential"

## v0.2 — trustworthy for real sites

- **Sync hooks** — jobs before/after a Sync: `artisan migrate`, `cache:clear`, `wp core update-db`, backups before upgrades
- **Notifications** — Slack, Discord, email, generic webhook on Sync failed / Degraded / recovered
- **Warn about relative bind mounts that hold data** (`./data` lives in the work dir and is lost on reboot)
- "Sync and recreate" in the UI; refresh from Git when an Application page opens

## v0.3 — many Applications, little repetition

- **ApplicationSet** — one template + generators (list, Git directories, Hosts) generate Applications
- **Image updater** — watch registries for new tags and commit the bump to Git
- **Follow the latest Git tag** — `targetRevision: "v1.*"` or a semver range (`>=1.4 <2.0`): deploy the newest matching tag automatically, instead of editing the Application for each release
- **Podman support** — run against Podman's Docker-compatible API socket (rootful and rootless): enable/check `podman-restart.service` so containers come back after a reboot, an SSH transport that doesn't need the Docker CLI on the server, rootless notes (ports < 1024, UID mapping on bind mounts), and the integration test suite running against Podman in CI
- Nested App of Apps
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

- Dependabot/Renovate for Go, npm and GitHub Actions
- Copy only changed files to remote Hosts
- A documentation site
