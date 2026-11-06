# v0.2 design decisions

The design review of [the original specification](original-spec.md), 2026-11-06. Each line is a question that
was put and the decision taken; [architecture.md](architecture.md) is the result. When something here changes,
change it here first.

## Scope and releases

- **Q1** No v0.1.0 release now; v0.2 is the redesign.
- **Q8** v0.1.0 is its own release from its own codebase; v0.2 breaks; 0.1.x gets fixes only. Both are released together.
- **Q18** No compatibility: no migration of v0.1's database, Hosts or work dirs; push and legacy code are deleted.
- **Q54** `release-0.1` branch for fixes (created from rc.10); v0.2 is built on `main`, one PR per ticket or
  milestone. Issue #9 (import and `../` references) stays unfixed in 0.1.

## Model

- **Q2** Pull-based like Argo; rootless Docker and non-root docker-group users must work.
- **Q9** One model: every Host runs an Agent; push (SSH/TLS) goes away; the Control Plane's machine gets a built-in Agent.
- **Q3** The Agent runs Compose itself; the Control Plane never talks to a Host's engine.
- **Q23** The Agent computes Live State, Diff, Health and drift and streams them; the Control Plane derives Sync Status and keeps History.
- **Q6** The Control Plane is reachable by Agents (public behind a proxy, or a private network); Agents only dial out.
- **Q7** Names: Host, Agent, Control Plane; **Q17** Source Server (not repo server).

## Secrets

- **Q4** Agent Hosts decrypt only on the Agent, with a Host Key born there (supersedes ADR 0002).
- **Q11/Q19** The Agent caches ciphertext only, decrypts in memory, and writes plaintext only to the app's folder in
  its configured work dir (RAM or disk); in-place file writes, not wipe-and-copy.
- **Q29** Secret sources plug in on the Agent; their credentials stay in the Agent's configuration; v0.2 ships
  SOPS+age. 1Password, LastPass, Vault, OpenBao, Ansible Vault come later as paid features.

## Source Server

- **Q5** A standalone component: isolation and rendering matter.
- **Q12** It builds the Desired State with secrets still encrypted: fetch, validate, render what needs no secret,
  package; the Agent decrypts and does the final Compose render.
- **Q13** It owns Git credentials and Git access; no database, keys or Docker; only the Control Plane may call it.
- **Q14** Agents get payloads through the Control Plane, never from the Source Server.
- **Q15** In-process by default, standalone with one setting; docs recommend standalone for production.
- **Q40** Credentials are passed through the Control Plane and stored sealed by the Source Server; Settings shows
  its status; the repository access check asks it.
- **Q45** Control Plane ↔ Source Server: gRPC with mTLS, certificates generated at install.
- **Q46** Polling, webhooks and App of Apps definitions all go through it.
- **Q31** The Control Plane keeps every payload it delivered; only new Revisions wait for the Source Server.

## Connection

- **Q16/Q20** One Agent-initiated WebSocket for everything (signals, payloads, status, logs); no separate HTTPS
  payload download. Argo's agent does the same (gRPC with a WebSocket fallback).
- **Q27** Protobuf messages now, gRPC transport later; content-addressed cache; only missing files travel.
- **Q22** Enrollment: single-use invite → the Agent generates an ed25519 signing key and its age Host Key →
  registers them; every connection signs a fresh challenge; no access/refresh tokens; revoke = delete the Host.
- **Q28** Extra layers: admin approval comparing the key fingerprint; a second live connection with the same key
  locks the Host; keys stored owner-only (optionally sealed with systemd-creds/TPM); one-click rotation and
  revocation; optional IP allowlist. No passphrase (it would break unattended restarts).
- **Q41** Protocol version handshake; Agents one minor behind supported; older refused, apps keep running; "Agent
  outdated" in the UI; self-update later.
- **Q51** Reverse proxies everywhere: WebSocket through any proxy, signed challenge works behind TLS termination,
  30 s pings, Source Server mTLS needs TLS passthrough, WebSocket stays as the fallback once gRPC exists, IP
  allowlist via trusted proxies, docs with Caddy/Traefik/nginx examples.

## Agent

- **Q10** Native systemd service (user unit for rootless, system unit with a docker-group user) and, **Q21**, a
  container image with `deploy/agent.yaml` (socket, path parity enforced, rootless sockets); never root by default.
- **Q24** No data loss: offline, the Agent keeps apps as deployed, restores after reboot from its cache, self-heals,
  never deploys new things, reports on reconnect.
- **Q25** Offline rollback `omegacd agent rollback` to cached Revisions, reported on reconnect, Auto-Sync turned off.
- **Q32** Cache keeps the last 10 Revisions per app (configurable); online Rollback reaches all of History.
- **Q48** The Agent keeps a small SQLite database; the work-dir ownership rules move to it.
- **Q57** (2026-11-06) An Application may name its own work dir (`workDir:`, absolute); empty means the Host's
  (`OMEGACD_AGENT_WORKDIR`), as before. The Agent writes only under roots its own configuration allows
  (`OMEGACD_AGENT_WORKDIR_ALLOW`, plus the Host's work dir), so Git or a breached Control Plane can't aim it at
  `/etc`; ownership rules and path parity hold in every root. A Child Application sets its own, like `host:`; an App
  of Apps deploys nothing and passes no defaults to its children.
- **Q58** (2026-11-06) Decrypted files are owned by the Agent's user, mode `0600`: no one else on the Host reads them,
  and containers running as root (rootful) or as the Agent's user (rootless) do. For images that read a secret file
  as a non-root user, `OMEGACD_AGENT_SECRET_GROUP=<gid>` makes them `0640` with that group, and the service joins it
  with `group_add:` (rootless engines show the Agent's user and group as root inside containers: `group_add: ["0"]`,
  verified on rootless Docker). Later: owner and mode per file, declared in Git next to the encrypted file. On macOS, Docker
  Desktop and OrbStack share host folders ignoring owner and mode: there any container reads them (Linux Hosts don't).
- **Q59** (2026-11-06) Plaintext stays in RAM, also across reboots with rootful Docker. After a reboot Docker restarts
  containers with a restart policy before the Agent runs, and creates their missing bind-mount sources as empty
  root-owned directories a non-root Agent can't replace. So the restore is split: the files (from the cache, decrypted)
  and then Compose. A native Agent writes the files before Docker starts (`omegacd agent restore --files`, a systemd
  unit ordered `Before=docker.service`); a containerised Agent, which starts after Docker, removes those placeholders
  through Docker with a short-lived container of its own image (local, so offline is fine; empty directories only, never
  files) and then writes the files. Rootless engines need neither. Anything left over is reported with the fix.
- **Q47** `import` goes only through the Agent (`--host NAME`); no SSH path.
- **Q34/Q52** Talks to any Docker-compatible socket; Podman rootless is in v0.2's test matrix; engine badges everywhere.

## Control Plane

- **Q26/Q35** Built-in Agent only in a full install; `OMEGACD_BUILTIN_AGENT=auto|true|false`, auto = on when a
  Docker-compatible socket is reachable and not in Kubernetes (`KUBERNETES_SERVICE_HOST`).
- **Q30** Disaster recovery: from backup, Agents reconnect; from nothing, re-enroll, apps show as "on the Host, not
  managed" and the App of Apps takes them over by name without redeploying unchanged ones.
- **Q36** History kept forever by default, optional retention.
- **Q38** Offline Hosts: last known state with a timestamp; Syncs queued until reconnect, a newer one replaces a queued one.
- **Q49** One admin in v0.2; GitHub SSO and roles in v0.3 (paid tiers).

## UI

- **Q37/Q43/Q53** Hosts are named when added (Git references them by name); cards show name and hostname; renaming
  only while no Application uses the Host.
- **Q37** Add Host flow with install commands, live wait, fingerprint approval; Host cards with status, versions,
  engine, rootless, keys and actions.
- **Q39** Badges: engine, Host offline, rolled back on the Host, queued Sync, Agent outdated.
- **Q42/Q44** Logs over the WebSocket; the web terminal is v0.3 and paid.

## Work plan

- **Q50** Done means: unit + integration with a real Agent against dind (rootful and rootless) + Podman rootless +
  `test/e2e` ported + chaos tests (kill Control Plane mid-Sync, kill Source Server, offline reboot, restore from
  backup and from nothing) + a real-world run (VPS and a Host behind NAT).
- **Q55** Milestones in order: 1 Foundations, 2 Agent core, 3 Connection, 4 Control Plane in pull mode, 5 UI,
  6 Install and import, 7 Hardening, 8 UI polish (then release).
- **Q56** ADRs 0003–0006 record the four hard-to-reverse decisions.
