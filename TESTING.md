# Testing

## Automated (runs in CI on every push)

| Layer | What | Command |
|---|---|---|
| Unit | controller (status, Auto-Sync, Self-Heal, retries, Rollback, App of Apps, restore, deletes), store, API, CLI, Git, secrets, crypto | `go test -race ./...` |
| Integration | real Docker: Sync, drift, Prune vs stop of undeclared services, network/volume pruning (in use, external, bind mounts), delete with volumes, content hash for configs/secrets/bind files, absolute bind mounts, force recreate, events; remote Hosts over SSH and TLS against `docker:dind` | `go test -tags integration ./...` |
| UI | type check, build, log/ANSI parser | `cd web && npm run check && npm test && npm run build` |
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
| 4 | Webhook | host the repo in Forgejo/GitHub, add the push webhook (`/api/webhooks/{provider}`) | push deploys within seconds, no polling wait | |
| 5 | Remote Host | add a VPS as a Host (SSH via Tailscale), move one app with `host:` | deploys there; files copied; health/logs work | |
| 6 | Fresh install | install from the README on a clean server with the release image | works by following the README only | |
| 7 | Token rotation | revoke the Git token on GitHub, then add a new one in Settings | access check/Sync fail clearly after revoke; work again after update | |

Not in scope for v0.1: load (many Applications), HA, multiple users, private registry pulls.
