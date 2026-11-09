# OmegaCD v0.2 architecture: pull-based Agents and a Source Server

The decided design for v0.2, the result of a design review of [the original specification](original-spec.md).
Every point here traces to a numbered decision in [decisions.md](decisions.md) (Qn); the vocabulary is in
[GLOSSARY.md](../../GLOSSARY.md); the work is split into [milestones](milestones/). v0.2 is a breaking
redesign: v0.1 (push over SSH/TLS) lives on in the `release-0.1` branch and gets fixes only (Q8, Q18, Q54).

## Goals

- **Pull, like Argo:** every Host runs an Agent that dials out to the Control Plane; nothing dials into a Host (Q2, Q9).
- **No data loss, ever:** if the Control Plane or the Source Server disappears, running Applications keep
  running, restart after reboots and keep their data (Q24, Q30, Q31).
- **Secrets stay on the Host:** an Agent decrypts with a key that was born there and never leaves (Q4, Q19).
- **Isolation:** Git credentials and rendering live in a separate Source Server; a breach of one part does not
  give away the others (Q5, Q13).
- **Performance is king:** one connection, compact messages, only changed files transferred (Q20, Q27).
- **Rootless and non-root:** the Agent never needs root; rootless Docker, docker-group users and Podman work (Q2, Q10, Q52).
- **Reverse proxies are normal** in front of the Control Plane and on Hosts (Q45, Q51).

## Components

All personas are one binary (Q5, Q15):

| Command | Persona | Runs where |
| --- | --- | --- |
| `omegacd server` | **Control Plane**: UI, API, database, sync history, Agent connections; with an in-process Source Server and, when Docker is present, a built-in Agent | the ops machine, a container, or Kubernetes |
| `omegacd source-server` | **Source Server**, standalone (recommended for production) | next to the Control Plane, internal network only |
| `omegacd agent` | **Agent** | every Host: native (systemd) or a container |
| `omegacd <anything else>` | CLI client (`app sync`, `import`, `keygen`, …) | laptops, CI |

```
            Git hosts                                 Hosts (anywhere, behind NAT/firewalls)
               ▲                                       ┌──────────────────────────────┐
               │ fetch (Git credentials)               │ Agent ── Docker/Podman socket │
   ┌───────────┴──────────┐  gRPC + mTLS  ┌──────────┐ │  │  cache (ciphertext only)   │
   │     Source Server     │◀─────────────│ Control  │◀┼──┘  work dir (plaintext, RAM  │
   │ fetch·validate·render │              │  Plane   │ │      or disk, configurable)   │
   └───────────────────────┘              │ UI · API │ └──────────────────────────────┘
                                          │ database │     one WebSocket per Agent,
                                          └──────────┘     always opened by the Agent
                                                 ▲         (through any reverse proxy)
                                                 │
                                           users, CI, webhooks
```

### Trust boundaries (Q4, Q13, Q14, Q40)

| | Git credentials | Host private keys | Docker access | Database |
| --- | --- | --- | --- | --- |
| Source Server | ✅ owns them (sealed with its own key) | ❌ | ❌ | ❌ |
| Control Plane | ❌ names and URL prefixes only | ❌ public keys only | ❌ | ✅ |
| Agent | ❌ | ✅ its own Host Key and signing key | ✅ its Host only | its own small one |

- A breached **Source Server** leaks Git credentials, no secrets, no Hosts.
- A breached **Control Plane** can tell Agents what to deploy, but can read no secret and no Git credential.
- A breached **Host** exposes only that Host's secrets.

## Source Server (Q5, Q12, Q13, Q15, Q17, Q31, Q45, Q46)

The only component that touches Git. It turns a Repository at a Revision into a **Desired State** with secrets
still encrypted:

1. **Resolve**: branch or tag → commit (polling and webhooks go through it: the Control Plane asks, it fetches).
2. **Render**: the files under an Application's path at that commit; App of Apps definitions parsed and validated.
3. **Validate**: `build:` refused; relative mounts not in Git refused; plain-text secrets in `environment:`
   reported as warnings; everything rendered that needs no secret (compose files merged, profiles applied,
   later templating). Interpolation that needs a SOPS `.env` happens on the Agent.
4. **Package**: a file list `(path, mode, sha256)` plus metadata (compose files, profiles, warnings), the files
   themselves served by hash.
5. **Access check and credentials**: the UI's repository access check and credential CRUD are served by it; the
   Control Plane passes credentials through and keeps only names and URL prefixes (Q40).

Interface first, transport second: in-process by default (`omegacd server`), standalone with
`OMEGACD_SOURCE_SERVER=https://…` over **gRPC with mutual TLS**, certificates generated at install. It accepts
only the Control Plane, never Agents or users. A reverse proxy in between must pass TLS through (TCP mode) (Q51).

## Control Plane (Q9, Q23, Q26, Q30, Q31, Q35, Q36, Q38, Q41, Q46, Q49)

- Holds Applications (each names its Host, Git is the source of truth), Hosts, invites, the sync history
  (kept forever by default, optional retention by count or age), settings, API tokens; one admin user in v0.2.
- **Sync**: ask the Source Server to render → store the files content-addressed → tell the Host's Agent "sync
  app X to Revision R, file list L" → record the Sync Operation from the Agent's reports.
- **Status**: Sync Status from target vs deployed Revision plus the Agent's drift report; Live State, Diff,
  Health and logs come from the Agent.
- **Offline Host**: shows the last known state with its time; a Sync is **queued** ("waiting for Host") and runs
  on reconnect; a newer Sync replaces a queued one.
- **Keeps every payload it delivered** (content-addressed, cheap): reinstalled Agents and Rollbacks to rendered
  Revisions work without the Source Server; only new Revisions wait for it, with the reason shown.
- **Built-in Agent**: `OMEGACD_BUILTIN_AGENT=auto|true|false` (default auto: on when a Docker-compatible socket
  is reachable and `KUBERNETES_SERVICE_HOST` is unset), enrolled automatically, otherwise a normal Agent. Only in
  a full install (Q26, Q35).
- **Disaster recovery** (Q30): from a backup (database + Master Key) Agents just reconnect; from nothing, Agents
  are re-enrolled, report what they run as "on the Host, not managed", and the App of Apps from Git takes those
  over by name without redeploying unchanged ones.

## Agent (Q3, Q4, Q10, Q19, Q21, Q24, Q25, Q29, Q32, Q34, Q48, Q52)

- **Runs Compose itself** against the local Docker-compatible socket (`DOCKER_HOST`): rootful Docker via the
  docker group, rootless Docker (`$XDG_RUNTIME_DIR/docker.sock`), Podman (rootless in v0.2's tests). Reports the
  engine, its version and rootless or not.
- **Never root** unless forced. Native: systemd user unit (rootless) or system unit with a docker-group user.
  Container: `deploy/agent.yaml` mounts the socket, the state volume and the **work dir at the same path inside and
  outside** (the engine resolves bind mounts on the host); the Agent checks path parity at start and refuses
  otherwise.
- **State dir** (`~/.local/share/omegacd-agent` or `/var/lib/omegacd-agent`): its signing key and age Host Key
  (owner-only files, optionally sealed with `systemd-creds`/TPM), a small SQLite database (apps, deployed
  Revisions, cache index, outbox of reports made while offline) and the **content-addressed cache** (ciphertext
  only; the last 10 Revisions per app by default, configurable).
- **Work dir**: each Application's folder, plaintext; on RAM (`/run`, `$XDG_RUNTIME_DIR`, the default) or disk
  (`/opt/services`) as configured. In RAM, decrypted secrets never touch a disk and are restored from the cache after a
  reboot; on disk they persist (owner-only) and the Agent warns about it at start. Files are written **in place** (single-file bind mounts keep their inode) and
  everything not in the Desired State is pruned; the ownership rules (never sync into a folder OmegaCD didn't
  create) live here (Q11, Q48). An Application may name its own work dir instead, under a root the Agent's
  configuration allows (Q57).
- **Decryption in memory only**: ciphertext from the cache → plaintext straight into the app's folder; no
  temporary plaintext anywhere (Q19). Secret sources plug in behind one interface: SOPS+age in the open-source
  Agent; 1Password, LastPass, Vault, OpenBao, Ansible Vault later (paid). Their credentials live in the Agent's
  configuration, never on the Control Plane or in Git, which holds references only (Q29).
- **Computes** Live State, per-service Diff and Health; watches engine events for drift; streams changes.
- **Offline autonomy**: keeps apps exactly as last deployed; restores them after a reboot from the cache (the files
  before Docker starts where it can, so plaintext stays in RAM with rootful Docker too, Q59); runs
  Self-Heal from the last Desired State; never deploys anything new and never deletes an app because the Control
  Plane is gone; reports on reconnect (Q24).
- **Offline rollback**: `omegacd agent rollback APP REVISION` to any cached Revision; recorded and reported on
  reconnect; the Control Plane shows it as rolled back on the Host and turns Auto-Sync off (Q25). Online, a
  Rollback reaches every Revision in History (Q32).
- **import**: `omegacd import --host NAME` asks that Host's Agent to read the running stacks; there is no SSH path
  (Q47).

## The connection (Q16, Q20, Q22, Q27, Q28, Q41, Q42, Q51)

- **One WebSocket per Agent**, always opened by the Agent (`wss://<control plane>/api/agents/v1/connect`), through
  any reverse proxy (HTTP/1.1 Upgrade); ping every 30 s; reconnect with backoff and jitter.
- **Messages in protobuf** in binary frames: hello and version, challenge and signature, desired state (file list),
  blob requests and chunks, status and drift reports, Sync Operation reports, logs, resync. A later gRPC transport
  swaps only the transport; the WebSocket stays as the fallback.
- **Only missing files travel**: the Agent asks for the hashes it lacks; a Rollback to a cached Revision sends
  nothing. Transfers resume by hash after a reconnect.
- **Resync**: on connect and every few minutes the Agent sends its per-app checksums; the Control Plane sends what
  differs (the fallback for missed signals).
- **Version handshake**: Agents one minor version behind are supported; older ones are refused with a message, and
  their apps keep running; the UI flags outdated Agents.
- **Logs** stream over the same connection. The web terminal is v0.3 and paid (Q42, Q44).

### Enrollment and authentication (Q22, Q28, Q43, Q53)

1. An admin adds a Host **with its name** (Git references Hosts by name); the UI shows the install commands with a
   single-use, short-lived invite (stored as a hash).
2. The Agent starts with the invite, generates an **ed25519 signing key** and its **age Host Key**, and registers
   both public keys; the invite is used up.
   On the wire (M3-02): the Control Plane sends a challenge; the Agent answers with Hello, `Enroll` (invite and age
   recipient, while it has an invite) and its signature over `omegacd-agent-v1 challenge\n` + nonce. A known key needs
   no invite. A refused Agent learns only that the invite is unknown, used or expired, and retries with backoff.
3. The UI shows the key fingerprint (also printed by the Agent; `SHA256:` + base64, as `ssh-keygen -l`); an admin
   compares and **approves**, sending the fingerprint compared: approval of a key that changed meanwhile fails. The
   waiting Agent stays connected and is sent nothing but `EnrollmentStatus` until then, and hears its approval at once.
4. Every connection authenticates by **signing a fresh challenge**: no bearer tokens to store or rotate. Works
   behind TLS-terminating proxies.
5. A second live connection with the same key **locks** the Host until an admin acts. Optional IP allowlist per
   Host (real client address via `OMEGACD_TRUSTED_PROXIES`). One-click key rotation and revocation.
6. A Host can be renamed only while no Application uses it.

## UI (Q34, Q37, Q38, Q39, Q40, Q43, Q53)

- **Add Host**: name → install commands (compose snippet, `install.sh` one-liner, systemd rootless or group) →
  live wait → fingerprint → Approve.
- **Host card**: name and hostname, online or last seen, Agent version, engine badge (Docker/Podman + version),
  rootless, OS/arch, number of apps, key fingerprint, Rotate key, Revoke, Locked with the reason, IP allowlist.
- **Badges everywhere** (tiles, app header, tree, lists, Host cards): engine, Host offline, rolled back on the Host,
  queued Sync, Agent outdated; tooltips with details.
- **Offline apps** show the last known Live State, Health, Diff and logs with "Host offline since …".
- **Settings → Source Server**: in-process or standalone, reachable, version, cache size; repository credentials
  (passed through); the access check asks the Source Server.
- **Decrypted files** show the key names the Agent reports (`KEY=••••`).

## Install (Q10, Q21, Q26, Q33, Q35, Q51)

- `install.sh` asks **full** (Control Plane, UI, Source Server, built-in Agent when Docker is there) or **agent**
  (Control Plane URL + invite; container or systemd, rootless detected).
- `deploy/compose.yaml` (full), `deploy/agent.yaml` (Agent; rootful and rootless variants), systemd units,
  `deploy/kubernetes.yaml` (full, no built-in Agent).
- Reverse proxy examples for Caddy, Traefik and nginx.

## Removed in v0.2 (Q9, Q18, Q47)

Push (SSH/TLS Hosts and the remote file helper), the `local` Host and `OMEGACD_LOCAL_HOST`, central decryption and
stored private Host Keys, the old work-dir layouts and their fallbacks, `import --ssh`. No migration from v0.1:
users reinstall.

## Later (not v0.2)

Web terminal, GitHub SSO, roles and audit (v0.3, paid tiers); secret sources beyond SOPS (paid); gRPC to Agents;
Agent self-update; Podman beyond the tested rootless setup; owner and mode of decrypted files per file, declared in
Git next to the encrypted file (Q58: v0.2 has `0600`, or `0640` with one group per Host). Editions: open source core, Teams, Enterprise, to be
decided (Q29, Q44, Q49).
