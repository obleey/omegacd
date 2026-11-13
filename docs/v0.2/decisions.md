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
- **Q9** One model: every Host runs an Agent; push (SSH/TLS) goes away; the Control Plane's machine runs one too,
  enrolled like any Host (Q104).
- **Q3** The Agent runs Compose itself; the Control Plane never talks to a Host's engine.
- **Q23** The Agent computes Live State, Diff, Health and drift and streams them; the Control Plane derives Sync Status and keeps History.
- **Q6** The Control Plane is reachable by Agents (public behind a proxy, or a private network); Agents only dial out.
- **Q7** Names: Host, Agent, Control Plane; **Q17** Source Server (not repo server).

## Secrets

- **Q4** Agent Hosts decrypt only on the Agent, with a Host Key born there (supersedes ADR 0002).
- **Q11/Q19** The Agent caches ciphertext only, decrypts in memory, and writes plaintext only to the app's folder in
  its configured work dir (RAM or disk); in-place file writes, not wipe-and-copy.
- **Q29** Secret sources plug in on the Agent; their credentials stay in the Agent's configuration; v0.2 ships
  SOPS+age. KeePass, 1Password, LastPass, Vault, OpenBao, Ansible Vault come later as paid features. Everything
  around secrets is per source, not SOPS-shaped: the Agent's `SecretSource`, the repository helper (M6-08), the
  "can't decrypt" report (M5-08).

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
- **Q73** (2026-11-08, design check before M3) An invite is valid for one hour and once; a new one is a click away.
- **Q74** Every new Host is approved by the admin comparing the fingerprint; no auto-approve, the Control Plane's own
  machine included (Q104).
- **Q75** A Host locked by a second connection with its key shows both connections (address, time) with two actions:
  unlock (a harmless cause, such as a cloned VM now gone) or revoke and add again (a key that may be stolen). Its
  Applications keep running meanwhile.
- **Q76** Key rotation is by hand, one click per Host; automatic rotation is later (roadmap).
- **Q77** In v0.2 keys are owner-only files (0600); sealing them with systemd-creds or a TPM is after v0.2 (roadmap):
  it needs testing per platform, and the test servers have no TPM. Q28's "optionally sealed" moves there.
- **Q78** The Agent sends its checksums (resync) on every connect and every 5 minutes, not configurable in v0.2: the
  safety net for lost signals, which carry changes at once.

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
- **Q60** (2026-11-07; **deferred to after v0.2** on 2026-11-08, see Q61) RHEL-family Hosts (RHEL, Rocky, Alma 9+,
  Fedora), with their defaults: SELinux **enforcing**, Podman (rootless) or Docker CE, dnf, firewalld. Containers read bind-mounted
  files there only with the `container_file_t` label: the Agent adds the **`z`** option (shared label: services of a
  project may mount the same file; `Z`, private to one container, would break that) to every bind mount whose source
  is inside the app's own folder, as Docker, Podman and Red Hat document. It never relabels anything else: absolute
  paths (the user's data) keep their labels, `:z` there is the user's choice; relabelling system paths can break the
  Host. `z` is a no-op without SELinux, so it's the same on every Host. Rejected: a `semanage fcontext` rule at install
  (for fixed package paths, fragile for folders the Agent creates and moves), `chcon` (undone by a relabel). Also
  planned then: `label=disable` on the Agent's helper containers (reclaim, import) and on containers that mount the
  Docker socket. A Rocky 9 VM with SELinux enforcing proved all of it during M2; with the narrowed scope (Q61) none of
  it is in v0.2 (code nobody can test on Debian/Ubuntu doesn't stay): it comes back with the RHEL family, M7-06 keeps
  what the VM taught.
- **Q61** (2026-11-07, narrowed 2026-11-08) Workloads run on Linux: in v0.2 Agents (Hosts) are supported on
  **Debian/Ubuntu, amd64 and arm64**, tested before every push on Debian 13 and Ubuntu 24.04, both architectures (real servers), with
  the same tests (`make test-linux`): v0.2 is the MVP. The RHEL family (Q60) follows
  after v0.2 (M6-07, M7-06 deferred): an emulated Rocky machine's Docker hung twice in a day and the gate grew past an
  hour, for Hosts no one runs yet; the SELinux handling and the test machines go with it. macOS is for development only: the CLI, a local Control Plane, trying an
  Agent; an Agent there warns at start (file sharing ignores owners and modes, so containers can read decrypted
  secrets; no systemd).
- **Q62** (2026-11-07) Logging everywhere, done early (M2-10) so it helps find bugs while v0.2 is built: every persona
  (Control Plane, Source Server, Agent, CLI) logs through `log/slog` with levels **error** (something failed, with
  what to do), **warn**, **info** (what happened: a Sync, a restore, a connection) and **debug** (the why: decisions
  like "kept container, same config hash and image", inputs, timings). `OMEGACD_LOG_LEVEL` (default info) and
  `OMEGACD_LOG_FORMAT` (text, json). Every entry carries `component` and, where they apply, `app`, `host`, `revision`,
  `op` (the Sync Operation), so one Sync can be followed across Control Plane, Source Server and Agent. **Secrets are
  never logged**, at any level (a test injects known secrets and scans every log line); URLs are logged redacted, and
  the logger replaces any value whose key names a secret (password, token, …) as a safety net. One exception, on
  purpose (user's choice, 2026-11-08): the admin password generated at first start without `OMEGACD_ADMIN_PASSWORD`,
  logged once and changed at first login.
  The CLI reads the same settings with `warn` as its default: it talks to people (at `debug` it shows its API calls).
- **Q47** `import` goes only through the Agent (`--host NAME`); no SSH path.
- **Q34/Q52** Talks to any Docker-compatible socket; Podman rootless is in v0.2's test matrix; engine badges everywhere.

## Control Plane

- **Q26/Q35** (superseded by Q104: no built-in Agent) Built-in Agent only in a full install; `OMEGACD_BUILTIN_AGENT=auto|true|false`, auto = on when a
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

## Later features (design check before M3, 2026-11-08)

The roadmap's items after v0.2, decided where they shape what M3 and M4 build: the protocol, who runs what, where it
is declared. Their details (syntax, UI, messages) are decided when each is built.

- **Q63** Only what is hard to change later is decided now. Protobuf fields are additive: none is reserved for these.
- **Q64** **Sync hooks** run **on the Agent**, so offline too, and are declared in the **Application definition**
  (API, CLI, UI, App of Apps), not in `compose.yaml`: Compose stays plain Compose, and one Compose folder deployed
  twice (staging, prod) can have different hooks. A hook names a service of the project and a command, and runs as a
  one-shot of that service (as `docker compose run --rm SERVICE CMD`: its image, environment, volumes, network), e.g.
  `preSync: [{service: db, run: [sh, -c, "pg_dump … > /backup/…"]}]`. They travel to the Agent as a field of the
  Desired State, stored with the Application, so an `omegacd agent rollback` runs the ones it last got. They run **only
  on a Sync or a Rollback**, never on a restore after a reboot, a Self-Heal or as init services (M2-11, which run on
  every `up`). Order: pre-sync hooks, init services, the new containers, Healthy, post-sync hooks. A failing pre-sync
  hook stops the Sync before anything changed; a failing post-sync hook makes it "Succeeded with warnings" (the new
  version runs); post-sync hooks run only once the Application is Healthy. What a hook does is the Application's
  developers' responsibility; OmegaCD guarantees when it runs and what a failure does.
- **Q65** **Rollback reverts code, not data.** Code is what Git holds for the Application: its Compose files, the files
  beside them, its encrypted secrets, and so the image versions the Compose files name. Data is what the Application
  wrote while running: named volumes, absolute data folders, a database's content and schema. OmegaCD never reverts
  data: a Rollback deploys the older Revision, whose migrations change nothing on a newer schema. The docs say so and recommend backward-compatible
  migrations (expand/contract: the previous version runs on the new schema); the safety net is a pre-sync backup
  hook, which also runs before a Rollback, so there is a backup from just before every change. Down migrations or
  restoring a backup are a person's deliberate step (`docker compose run --rm …`); an explicit restore hook is an idea
  for the roadmap, not automatic.
- **Q66** **Order between Child Applications** (`dependsOn`): the **Control Plane** orchestrates, the one that sees every
  Host: a child is synced once those it depends on report Synced and Healthy; one on an offline Host makes its
  dependents wait, shown ("waiting for prod-1"), like queued Syncs (Q38). The Agents' reports already carry this.
- **Q67** **Restart unhealthy containers** is Self-Heal **on the Agent**, offline too; its setting is a field of the
  Desired State.
- **Q68** **Image updater**: the **Source Server** commits the tag bump, with an optional, separate **write** credential
  per repository used only for it; read-only stays the default (Q13, Q40). Neither the Control Plane nor an Agent
  ever gets write access to Git.
- **Q69** **Follow the latest Git tag**: `targetRevision` may be a semver range (`~1.4`, `v1.*`), resolved by the
  Source Server to the newest matching tag; a pushed tag refreshes through webhooks or polling. The Source Server's
  interface keeps a Revision as a string (M1).
- **Q70** **ApplicationSet**: the **Source Server** expands generators, like App of Apps definitions (Q46); for a Host
  generator the Control Plane passes the Hosts in with the render. The Source Server never calls the Control Plane.
- **Q71** **Notifications** come from the **Control Plane**, from the Agents' reports; what failed while a Host was
  offline arrives in its outbox and is notified once, on reconnect, with the time it happened and that the Host was
  offline. Agents never notify (no credentials on Hosts).
- **Q72** SSO, RBAC, ApplicationSet's UI, Swarm and self-upgrade touch only the Control Plane: nothing in M3. Agent
  self-update stays later (Q41).

## Control Plane in pull mode (design check before M4, 2026-11-09)

- **Q79** M3-06 (resync, outbox drain) moves after M4-01 and depends on it: it heals real Syncs and drains real Sync
  Operations, end to end through the binary, instead of stand-ins.
- **Q80** Agents sit behind the controller's existing engine seam (`domain.Engine`): the controller and its tests stay.
  Push Hosts (SSH/TLS) and Agent Hosts coexist, chosen per Host, until M4-05 deletes push; `local` uses the Docker
  socket until M4-05 deletes it (Q104). M4-01 refined it: a Sync to an Agent Host branches right after Render, since
  the engine seam works on a folder the Control Plane writes and decrypts; the files go to the Agent as rendered,
  secrets still encrypted. Diff, Live State, logs and resources stay behind the seam until M4-02 moves them.
- **Q81** A Sync to an offline Host is a Sync Operation "Waiting for Host", kept in the database (it survives a Control
  Plane restart). No separate queue: on reconnect the resync (M3-06) finds the difference and delivers it. A newer
  Sync supersedes a waiting one (Q38). So does a rollback run on the Host (Q25), reported from its outbox (M3-06): the
  later, deliberate act; the waiting Sync would undo it on reconnect.
- **Q82** Self-Heal always runs on the Agent, online too; the Control Plane no longer self-heals and records the
  Agent's reports as Sync Operations with trigger `self-heal`. One implementation, faster, the same History.
- **Q83** Deleting an Application goes through the Agent with a new `RemoveApp` message (delete volumes, keep
  containers), so `app delete`, `--volumes` and `--cascade=false` keep their meaning. To an offline Host it waits like
  a Sync: the Application shows "Deleting, waiting for Host" and leaves the Control Plane only once its Agent confirms.
- **Q84** Registry credentials stay on the Control Plane (Settings unchanged).
- **Q85** The Agent asks for a registry's credentials when it is about to pull from it (image names may come from an
  encrypted `.env` only the Agent reads); the Control Plane answers only for Hosts it serves an Application to.
- **Q86** No plaintext anywhere: the answer is age-encrypted to the Host's Host Key (its public half from enrollment),
  so it is ciphertext on the wire whatever the transport or proxy; sealed at rest on the Control Plane; on the Agent
  decrypted in memory for that pull only, never written; never in any log.
- **Q87** Live Sync progress (pulling, creating, started, …) is streamed by the Agent in a new message, so the UI's
  live progress view keeps working.
- **Q88** (superseded by Q104) The built-in Agent's Host is `local` (the Control Plane's machine, today's name and default); its state dir
  is in the Control Plane's data dir, its work dir `OMEGACD_WORKDIR`.
- **Q89** (superseded by Q104) The built-in Agent may run as the Control Plane's user, root included (the image runs as root today), with a
  warning: it holds the Docker socket anyway. M6-02 makes the image run as a non-root user in the socket's group.
- **Q90** After M4-05's fresh schema, the Control Plane refuses a v0.1 database with a message (export the Application
  definitions with v0.1's `app export`, then reinstall) and leaves the file untouched (Q18).
- **Q91** Resync never deletes: Applications on a Host the Control Plane doesn't know are "on the Host, not managed"
  (Q30) and keep running; only `RemoveApp` removes anything from a Host.
- **Q92** `app delete --forget` (and the UI's delete dialog) removes an Application whose Host is gone for good from
  the Control Plane only, after a confirmation that its containers, if any still exist, are left alone.
- **Q93** A Sync whose connection drops finishes on the Agent, and its result arrives through the outbox on reconnect;
  meanwhile it shows "Running, Host disconnected", never failed.
- **Q94** M4-01 is split: M4-01 Sync through Agents (files, blob transfer, Sync Operations, waiting for the Host,
  Rollback, Self-Heal reports, Q95); new M4-08 deletes through Agents (Q83, Q92), registry credentials on request (Q85,
  Q86), live progress (Q87). Order: M4-01, M3-06, M4-08, M4-02, then the rest of M4.
- **Q95** The Agent trusts a Control Plane only over TLS: it refuses `http://` except on loopback (`localhost`,
  `127.0.0.1`, `::1`: development and the built-in Agent), since over plain HTTP anyone in between could pose as the
  Control Plane and deploy to the Host; the Agent's signature proves the Agent, only TLS proves the Control Plane.
  `OMEGACD_AGENT_CA_FILE` trusts a private CA. Part of M4-01, the first ticket where the Control Plane sends what
  deploys.

## Coverage (2026-11-09)

- **Q96** Coverage is **100% of all reachable code, existing code included**, measured combined: unit, integration
  and e2e (the binary built with `go build -cover`, its coverage collected while the e2e scripts drive it), generated
  protobuf code excluded. A line no test can reach (an error that can't happen, such as encoding a fixed struct into
  memory or a healthy SQLite transaction failing) is marked in the code with a comment `coverage: unreachable, <reason>`
  on its block's first line (M4-09: a marker moves with the code and is reviewed with it, where a list of line numbers
  would go stale); any other uncovered line fails CI. No fault-injection hooks are added to production code just to reach
  such lines. M4-09 builds the measurement and the CI check (at first: a PR's changed lines, and no drop in total);
  M4-10 brings the existing code to 100%, after which CI checks all of it. (Refined the same day: first only new code
  was required; the user wants all code.)

## v0.1 (2026-11-09)

- **Q97** v0.1 is deprecated. v0.2.0 is a new OmegaCD and its first stable release; there is no v0.1.0:
  `v0.1.0-rc.10` stays v0.1's last release, marked deprecated with the `release-0.1` branch, which is frozen (no
  fixes). v0.1-only code gets no tests or features: M4-05 deletes it, and M4-10's coverage starts after that. What
  v0.1 still had to test on a real VPS moves to v0.2's real-world run (M7-04). Supersedes Q54's "fixes only" branch.

## Status from Agents (2026-11-09)

- **Q98** M4-02 is split: M4-02 has the Agent report Health, Live State, drift and resources as they change, the
  Control Plane keeping the latest report for the last-known state offline, and logs on demand; new M4-11 has the
  Agent compute the Diff of a Revision not yet deployed, staged in its work dir (RAM, Q19) and never deployed. Order:
  M4-02, M4-11, then M4-03.

## Webhook providers (2026-11-10)

- **Q99** Webhooks keep their native formats (GitHub, GitLab, Gitea/Forgejo) and add one **generic** endpoint,
  `POST /api/webhooks/generic` with `Authorization: Bearer <OMEGACD_WEBHOOK_SECRET>` and `{"repoURL": "…"}`, for any
  other caller: a CI step after a push, a script, a Git host without a format here. Bitbucket, Azure DevOps and Gogs
  get no native parser: their payloads are fixed, so they use polling or a CI step calling the generic endpoint. A
  body that isn't the provider's format answers 400 (before: 401, like a wrong signature). Part of M4-03.

## Built-in Agent (2026-11-10)

- **Q100** (superseded by Q104) The built-in Agent has its own Host Key, generated in its state dir (`<data>/agent`) like any Agent's, and
  is enrolled and approved as the Host `local` at every start, without an invite (the Control Plane made both keys
  itself). The Master Key then only seals the database and decrypts no Application's secret (Q19); secrets for
  `local` are encrypted to the built-in Agent's recipient, shown with the Host. Nothing carries over: v0.2 starts
  fresh (Q90, Q97). Part of M4-04.
- **Q101** (superseded by Q104) With the built-in Agent off (`OMEGACD_BUILTIN_AGENT=false`, or auto without an engine or in Kubernetes),
  `local` deploys from the Control Plane as before until M4-05 deletes that path and `OMEGACD_LOCAL_HOST` with it;
  from then on, off means there is no `local` Host. M4-04 only adds.
- **Q102** `adopt` may be changed on an existing Application. An Agent checks for a hand-started project of the same
  name at its first deploy of the Application, not at create (the Control Plane can't look at the Host), so a refused
  first Sync is answered by setting adopt and syncing again; once the Agent has deployed it, adopt changes nothing.
  Before: create-only, the old path refusing at create.
- **Q103** One machine, one Agent, one Host. The machine is provisioned with its engine (rootful Docker, rootless
  Docker or Podman) and its Agent uses that one: `DOCKER_HOST` when set (checked at start), otherwise the first socket
  found of rootful Docker, rootless Docker, rootless Podman and rootful Podman, the log naming it and any others. Several
  Agents on one machine are not supported.

## No built-in Agent (2026-11-11)

- **Q104** No built-in Agent: the Control Plane's machine is a Host like any other, with an `omegacd agent` installed
  beside the Control Plane and enrolled with an invite under the name the admin gives it; `omegacd server` never talks
  to a container engine. Every Application names its Host (`host:`): there is no default Host and no Host `local`
  (M4-05 deletes them with `OMEGACD_LOCAL_HOST` and the Control Plane's own deploy path). `install.sh full` installs
  the Control Plane and Source Server only; an Agent on the same machine is installed and enrolled with `install.sh
  agent`, as on any Host. Why: the Control Plane serves the UI and API and should not hold the engine's socket, which
  is root on the machine (Q89 only warned); one enrollment and one code path, without `local`'s special cases (M4-04
  found one: the built-in Agent turned off left `local` waiting for it); nothing to switch off in Kubernetes; and
  OmegaCD manages its own Control Plane as an Application on that Agent, which an Agent inside the server could not
  (a Sync of its own project would restart it mid-deploy). If the Control Plane restarts during that Sync, the Agent
  finishes it and reports it on reconnect (M3-06). An Agent refuses only its own Compose project (M4-05).
  Supersedes the built-in Agent of Q9, Q26/Q35, Q74, Q88, Q89, Q100 and Q101, and ADR 0003's consequence. M4-04 keeps
  what every Agent needs (Q102, Q103) and runs the e2e scripts through an enrolled Agent.

## Before M4-05: deleting push (2026-11-11)

- **Q105** A fresh v0.2 database schema, one `CREATE` instead of v0.1's migrations, with its own version marker. Any
  older database, v0.1 or v0.2 from before M4-05, is refused with Q90's message and left untouched; v0.2 is not
  released, so there is no one to migrate. The repository credentials the Control Plane kept before M1-06 go with it.
- **Q106** Git over SSH stays: the Source Server clones with a deploy key saved as a repository credential, verifying
  the server against `OMEGACD_SSH_KNOWN_HOSTS`, which is now for Git only. HTTPS tokens alone would lose deploy keys
  (GitHub has no HTTPS equivalent not tied to a person). The image's `openssh-client` goes: it served only `ssh://`
  Hosts, and go-git needs none. After v0.2 (ROADMAP), as Argo CD does: built-in host keys for the big Git providers,
  known hosts managed through the CLI and UI, and GitHub App credentials.
- **Q107** The engine package is the Agent's own: every push trace goes (SSH/TLS Hosts and their fields and settings,
  the remote file helper, the engine's `host` parameter, the Control Plane's engine path and its `Engine`,
  `OMEGACD_WORKDIR` and `OMEGACD_LOCAL_HOST`). The Control Plane reaches every Host through its Agent.
- **Q108** One public Host Key recipient per Host, the one its Agent reported at enrollment, shown with its
  `.sops.yaml` rule and used by `import`. Stored private Host Keys and `host key generate|import|remove` go (Q9).
  Rotating an Agent's Host Key, the previous one kept for decryption until files are re-encrypted, is a ticket after
  v0.2 (ROADMAP).
- **Q109** An Agent refuses only the Compose project that contains its own container (found as `SelfID` was: the
  container ID from the hostname); an Agent under systemd refuses nothing. The Control Plane's project is an
  Application like any other (Q104).
- **Q110** `local` is an ordinary Host name. M4-05 edits the README to be correct (push, `local` and the Control Plane's
  Host Keys gone; "Your first Application" installs and enrolls an Agent first), and strips the deploy files of what the
  server no longer uses (the Docker socket, the work dir, `OMEGACD_LOCAL_HOST`); the full rewrite comes with M6.
  `import --ssh` goes (Q47); `import` without it stays until M6-05. One PR, deleting the UI parts that call removed
  endpoints, with no new UI (M5 builds the Add Host flow).

## Ubuntu 26.04 (2026-11-12)

- **Q111** v0.2 is tested on Ubuntu 24.04 (and Debian 13), not 26.04: every CI job is pinned to `ubuntu-24.04`
  (`ubuntu-24.04-arm` for arm64), so a runner image change can't move the tests under us. On GitHub's 26.04 runner,
  rootless Podman couldn't remove a container's network (cause not found yet); Ubuntu 26.04 as a tested Host is after
  v0.2.0 (ROADMAP), with what M4-12 found. M4-12 is deferred.

## History and disaster recovery (design check before M4-06, 2026-11-13)

- **Q112** History is kept forever unless `OMEGACD_HISTORY_KEEP=N` (the newest N Sync Operations per Application)
  or `OMEGACD_HISTORY_MAX_AGE` (a Go duration) is set (Q36). Pruned at start and hourly, logged with how many went.
  An Application's deployed Sync Operation and its last successful one are always kept: Rollback and the deployed view
  need them.
- **Q113** M4-07's store of delivered payloads (Q31) prunes with the history, a payload going when no kept Sync
  Operation uses it.
- **Q114** `omegacd backup FILE` writes one tar (mode 0600, never over an existing file): a consistent `omegacd.db`
  (SQLite's `VACUUM INTO`, the server running) and, with the Source Server in-process, its still encrypted
  `credentials.age`. No private key is in it. Only the admin may take one (`GET /api/backup`), logged at info, never
  its contents. Restoring is documented steps, not a command: an empty data dir, `tar -xf`, the keys back in place,
  start.
- **Q115** The admin backs up the Control Plane's two keys themselves: the Master Key (`age.key`) seals only the
  registry credentials' passwords, `source.key` only the Git credentials; losing one means entering those credentials
  again. `omegacd backup` and the README say so. The Master Key is generated at the first start when missing (as
  `source.key` is), logged with its path: no `age-keygen` install step.
- **Q116** After a restore the database can be older than its Hosts. When an Agent resyncs, a Sync Operation still
  Running for one of its Applications that the Agent neither deploys now nor reported (its outbox arrives before its
  checksums) is closed as Unknown, "result unknown", logged. The Agent's resync names the Sync Operations it is
  deploying. The same closes a Sync whose Desired State was lost with a dropped connection (the hub resends one only
  within a connection), which until then stayed Running.
- **Q117** An Agent an invite enrolls clears its outbox first, logging how many reports it dropped: the Control Plane
  that enrolled it knows none of those Sync Operations (after a recovery from nothing, a report for an old op 5 would
  otherwise close a new, unrelated op 5).
- **Q118** "On the Host, not managed" (Q30, Q91): per Host, the Applications its Agent runs that the Control Plane
  doesn't know, refreshed at every resync, shown by `host list`, `host get` and the API (the UI comes with M5-02),
  never acted on.
- **Q119** An App of Apps (or anyone) creating an Application over one its Host's Agent already runs at the target
  Revision takes it over without deploying: its first Sync, finding its Agent's checksum of the deployed tree (sent with
  every resync, kept per Host) equal to the target's, sends nothing and records one Sync Operation, Succeeded, "taken
  over: its Agent already runs this Revision"; no container is touched, and history, Rollback, the deployed Revision
  and retention work as after any Sync. At another Revision it Syncs normally, Compose recreating only what changed.
  (Refined while building M4-06: first "no Sync Operation", but the Control Plane counts as deployed only what a Sync
  Operation recorded.)

## Payloads without the Source Server (design check before M4-07, 2026-11-13)

- **Q120** The Control Plane keeps each render's file list, by tree checksum (`renders` in its database): a Sync
  records the render it made, and deploys from it without rendering again. A Sync Operation an Agent ran itself
  (restore, self-heal, rollback on the Host) reports the same checksum, so it finds the render too.
- **Q121** The files themselves go in the Control Plane's own blob store (`<data>/payloads`, apart from the in-process
  Source Server's `blobs`, which it collects as a cache): before sending a Desired State it copies the ones it lacks
  from the Source Server, and a Sync whose files can't be copied fails before the Agent gets anything. Agents' blob
  requests and the Diff's compose text read that store first. With the Source Server in-process its files are stored
  twice, accepted for one code path.
- **Q122** Rollback, and a Sync that waited for an offline Host, deploy the stored render of their tree; only without
  one is the Source Server asked. An App of Apps renders through the Source Server: its children's definitions are read
  from Git.
- **Q123** A resync that shows an Application deployed (a Succeeded Sync Operation) that its Agent doesn't run, not
  being deleted and with no Sync Operation Running or Waiting, deploys its deployed Revision again from the stored
  render: trigger `restore`, whatever Auto-Sync says, "its Agent didn't run it (reinstalled?)". It restores what the
  Control Plane already deployed, nothing new.
- **Q124** With the Source Server unreachable a new Revision gets no Sync Operation: `ErrUnavailable` (HTTP 503), the
  status saying "Source Server unreachable … the deployed Revision keeps running". Polling and webhooks find it once
  it is back, Auto-Sync runs it; a manual Sync is clicked again.
- **Q125** Payloads prune with the history (Q113): hourly, after the retention, renders no Sync Operation uses, then
  blobs no render uses (an hour's grace), also with history kept forever: a deleted Application's payloads go.
- **Q126** `omegacd backup` holds the renders (in the database), not the blobs: after a restore Agents with their
  cache roll back without the Source Server; a reinstalled Agent needs it until its files are copied again.
- **Q127** A reinstalled Agent gets back on its Host by its keys: the README has the operator back up its state dir's
  `signing.key` and `host.key`, and a reinstall that puts them back is a plain reconnect, no enrollment, Encrypted
  Secrets still decrypting; its Applications come back by Q123, the files from the Control Plane's store. A Host with
  Applications whose keys are lost has no way back in v0.2 (`host add` refuses an enrolled Host, `host delete` one with
  Applications). (Found while building M4-07.)
- **Q128** A Host Key that is lost can't decrypt the Encrypted Secrets in Git: a restore fails with the Agent's
  decryption error naming the file. No code: the README says to back the key up, or to re-encrypt for the new one
  (`host key show`); M5-08 shows it in the UI.

## Coverage and no silent path (design check before M4-10, 2026-11-13)

- **Q129** M4-10 is split into sub-tickets by package, one PR each (M4-10a–e); each brings its packages to 100% and no
  silent path, the last turns on CI's 100%. 713 uncovered blocks at the start (88.0%): one PR would not be reviewable.
- **Q130** What a test reaches in a line or two is tested: a closed store, a read-only or missing folder, an existing
  fake. Marked unreachable is only what no test can reach: a statement in a transaction after an earlier one succeeded,
  `crypto/rand`, marshalling plain structs, a value just validated (Q96: no fault-injection hooks).
- **Q131** A decision that logs at debug (Q62) is a branch that changes what the system does: it skips, retries, waits,
  picks a Revision, refuses or falls back; it says why. Pure functions (parsers, formatters, validators) don't log:
  their errors say why.
- **Q132** Every Warn and Error an operator acts on (server, Source Server, Agent) has a test checking its message;
  errors only returned upward, and debug lines, are covered but not asserted.
- **Q133** At the end CI's Coverage job and `make gate` fail on every uncovered block not marked (`-all`), floor 100.
  No gate-only exceptions: a line only Debian or a gate target reaches gets a test CI's Ubuntu runners run.
- **Q134** From the review before M4-10: the Control Plane refuses a database newer than it knows, as the Agent does;
  the Application name rule lives in `domain` and the Agent refuses a Desired State, Diff or removal naming an
  Application or App of Apps against it (a Failed Sync, not a dropped connection); an unset `OMEGACD_WEBHOOK_SECRET` is
  warned about at start; `emit` logs a dropped event at debug; `omegacd import` says when it falls back to `main`; a
  `du` size it can't read is unknown, not 0; the silent `ListApps` and `AgentStatus` errors are logged or returned to a
  caller that logs. (`ListCredentials` turned out not silent: its list is in memory, and a wrong `source.key` stops the
  start.)
- **Q135** Q96 measures the Go code; the SvelteKit UI keeps its own check and tests, its coverage left to M8 if wanted.
