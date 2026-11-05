# Testing

## Automated (runs in CI on every push)

| Layer | What | Command |
|---|---|---|
| Unit | controller (status, Auto-Sync, Self-Heal, retries, Rollback, App of Apps, restore, deletes, decrypted-file list, plain-text secret warnings), store, API (incl. gzip), CLI (incl. `keygen`), Git, secrets, crypto, bind-mount validation | `go test -race ./...` |
| Integration | real Docker: Sync, drift, Prune vs stop of undeclared services, network/volume pruning (in use, external, bind mounts), delete with volumes, content hash for configs/secrets/bind files, absolute bind mounts, force recreate, events; remote Hosts over SSH and TLS against `docker:dind` | `go test -tags integration ./...` |
| UI | type check, build, log/ANSI parser, line diff, syntax colors (Shiki) | `cd web && npm run check && npm test && npm run build` |
| Security | secret scan of the full Git history | gitleaks (CI) |

## Real-world checklist (v0.1)

Run against a real OmegaCD with a real stack (the reference stack is an App of Apps with Caddy, WordPress + MariaDB,
Forgejo, Umami + Postgres, Uptime Kuma, Prometheus + Grafana). Record the date, version and result.

| # | Test | Steps | Expected | Result |
|---|---|---|---|---|
| 1a | Config file change | edit `apps/caddy/Caddyfile`, push | caddy OutOfSync → auto-synced → container recreated (content hash) | ✅ 2026-11-04 dev-88eebb2 — recreated, header live |
| 1b | Image version bump | bump `wordpress` image tag, push | Diff shows `wordpress changed (image)`; synced; new version running | ✅ 2026-11-04 — diff `~ wordpress: image`, 7.1.3 running, DB untouched |
| 1c | Secret change | `sops edit` a `*.sops.env` (e.g. Grafana admin password), push | app recreated; new value in effect; UI shows only `KEY=••••` | ✅ 2026-11-04 — ciphertext in Git, value in container, UI `KEY=••••`, Prometheus untouched |
| 1d | Profile on/off | add profile `debug` to wordpress, then remove it | Adminer starts / is stopped (Prune off), "prune pending" shown | ✅ 2026-11-04 — on via App of Apps Sync; off → Adminer stopped, `prunePending`, volumes kept |
| 1e | Absolute host path | mount `/abs/path:/data` in an app; write from container; force a recreate; turn on Prune + …also volumes | files visible both ways, survive recreate and Prune, never drift | ✅ 2026-11-05 dev-bc34e79 — Uptime Kuma `/Users/obleey/omegacd-data/kuma-backup`: both directions, survived recreate and Prune+volumes; all 9 volumes intact |
| 2a | Add an Application | add a definition to `platform/apps.yaml`, push, Sync the App of Apps | child created and deployed | ✅ 2026-11-04 — `+ whoami` in diff, created and Healthy |
| 2b | Remove (Prune off) | remove the definition, push, Sync | child kept running, flagged "not in Git" | ✅ 2026-11-04 — kept running, parent OutOfSync "not in Git" |
| 2c | Remove (Prune on) | Prune on for the App of Apps, Sync | child and its containers deleted, volumes kept | ✅ 2026-11-04 — app and containers deleted, parent Synced |
| 3a | Self-Heal | `docker stop` a container of an app with Self-Heal | restarted automatically within seconds | ✅ 2026-11-04 — Self-Heal restarted it within seconds |
| 3b | Rollback | Rollback an app to an earlier Revision | old Revision running, Auto-Sync off, banner shown | ✅ 2026-11-04 — child Rollback refused with guidance; parent Rollback + return OK (found and fixed: stale "rolled back" flag) |
| 3c | Docker engine restart | restart Docker/OrbStack cleanly | containers come back; UI updates without a page reload | ✅ 2026-11-04 — OrbStack stop/start: Docker back in 15 s, OmegaCD resynced, all Healthy; UI shows one "Reconnecting…" banner, no error toasts (found and fixed: stale list, error toasts) |
| 3d | Work dir moved | stop OmegaCD, start it with a new empty `OMEGACD_WORKDIR` | every Application restored from its deployed Revision, containers recreated with mounts in the new dir, data kept | ✅ 2026-11-05 db2209e — 6 `restore` Syncs in 15 s, all 9 containers recreated with mounts in `~/Documents/learn`, volumes and Kuma backup intact, all sites up |
| 3e | Upgrade leaves no `*.sops.*` copies | start a build that deletes them on a work dir written by an older one | encrypted copies removed, decrypted files still masked, nothing synced or recreated | ✅ 2026-11-05 bfb981a — 5 copies removed, lists written, all 9 container IDs unchanged |
| 4 | Webhook | host the repo in Forgejo/GitHub, add the push webhook (`/api/webhooks/{provider}`) | push deploys within seconds, no polling wait | ✅ 2026-11-05 main — local Forgejo 17, private repo with a token credential, polling at 1h: pushes deployed in 12–15 s incl. the image pull (3 runs), forged signature refused (401) (found and fixed: `OMEGACD_POLL_INTERVAL=0` crashed OmegaCD; now turns polling off) |
| 5 | Remote Host | add a VPS as a Host (SSH via Tailscale), move one app with `host:` | deploys there; files copied; health/logs work | |
| 6 | Fresh install | on a clean server: `curl -fsSL …/deploy/install.sh \| OMEGACD_VERSION=0.1.0-rc.2 sh`, then follow the README | running; printed password logs in; works by following the README only | |
| 6a | Install script (Compose) | unattended, then again; once with `age-keygen` and `omegacd` not on PATH | key created (also via Docker only), password logs in, a re-run keeps the Master Key | ✅ 2026-11-05 rc.1 image on OrbStack — all three (found and fixed: rc.1 image has no `keygen`, Alpine fallback) |
| 6b | Install script (Kubernetes) | unattended with `OMEGACD_KUBE_CONTEXT` | namespace, key Secret, rollout; login through port-forward; `local` Host logged once as unreachable | ✅ 2026-11-05 OrbStack Kubernetes v1.36 |
| 7 | Token rotation | revoke the Git token on GitHub, then add a new one in Settings | access check/Sync fail clearly after revoke; work again after update | ✅ 2026-11-05 rc.2 + main — Unknown with the reason after revoke; Synced again after updating the credential (found and fixed: Refresh of the App of Apps didn't refresh its children; tiles didn't say why an app is Unknown) |
| 8 | Control plane in Kubernetes | OmegaCD in OrbStack's Kubernetes with the Docker socket, the same database and work dir as before (`hostPath`) | takes over the running apps: no restore, no recreate; UI via LoadBalancer | ✅ 2026-11-05 `ghcr.io/obleey/omegacd:0.1.0-rc.1` (public image) — all 9 container IDs unchanged, all sites up |
| 9 | Adopt running stacks | stacks started by hand with `docker compose up` (one with images + named volumes only, one mounting a config file from its folder); put the same compose files in Git, create the Applications with `adopt: true`, Sync | without `adopt` refused; images-only stack keeps its containers (no downtime); a stack mounting files is recreated once (the Diff shows `~ config` beforehand), same config, named volumes kept | ✅ 2026-11-05 main, locally — as expected (found and fixed in the test: IPv6 `localhost`). ⏳ on the VPS with real stacks |
| 10 | Import running stacks (`omegacd import`) | in a repo checkout: `omegacd import --ssh user@vps --host vps`, review the diff, push, App of Apps with path `platform`, Sync each app | config in Git, data paths absolute on the host, secrets only encrypted (env files, Compose secrets, passwords moved out of `environment:`), definitions with `adopt: true`; takeover as predicted | ✅ 2026-11-05 main, automated: unit (fake host) + integration against real Docker, locally and over SSH to a separate Docker host (found and fixed: the SSH half of the remote Host tests had been skipping silently). ⏳ on the VPS |

Not in scope for v0.1: load (many Applications), HA, multiple users, private registry pulls.
