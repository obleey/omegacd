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

## v0.2 — trustworthy for real sites

- **Sync hooks** — jobs before/after a Sync: `artisan migrate`, `cache:clear`, `wp core update-db`, backups before upgrades
- **Notifications** — Slack, Discord, email, generic webhook on Sync failed / Degraded / recovered
- **Restore after a remote Host reboots** (today only the OmegaCD machine restores itself)
- **Warn about relative bind mounts that hold data** (`./data` lives in the work dir and is lost on reboot)
- "Sync and recreate" in the UI; refresh from Git when an Application page opens

## v0.3 — many Applications, little repetition

- **ApplicationSet** — one template + generators (list, Git directories, Hosts) generate Applications
- **Image updater** — watch registries for new tags and commit the bump to Git
- Nested App of Apps
- Theme picker (all Catppuccin flavors)

## v0.4 — teams

- Multiple users, SSO (OIDC), RBAC (e.g. access per customer)
- Audit log
- More Secret Sources: Vault, 1Password, Bitwarden
- Postgres as an alternative to SQLite

## Later

- Docker Swarm
- Agent mode for Hosts behind NAT or firewalls
- Web terminal into containers
- OmegaCD upgrading itself

## Housekeeping

- Dependabot/Renovate for Go, npm and GitHub Actions
- Copy only changed files to remote Hosts
- A documentation site
