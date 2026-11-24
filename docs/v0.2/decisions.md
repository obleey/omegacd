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
  logged once and changed at first login. (Gone with Q300: the first password is in a file, the log says only where.)
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

## Source, workdir, secrets, appdef (before M4-10b, 2026-11-13)

- **Q136** The standalone Source Server logs its failed requests where every handler's error passes (`toStatus`):
  Warn for what the caller didn't cause (Git, the disk), debug for not found, invalid and already exists, which the
  Control Plane reports. Messages are already without secrets (`hide`).
- **Q137** The Control Plane's Source Server client logs a lost Invalidate at debug (the next Resolve may use a fetch
  up to `FetchCooldown` old); `domain.Source.Invalidate` keeps returning nothing.
- **Q138** Rendering logs at debug what it doesn't check: compose files that don't load without their secrets (the
  Host's render checks them) and files the plain-secret warning can't read.
- **Q139** Diff's errors name the Repository and Revisions; an error from a damaged repo cache names its folder, to
  delete. Repairing it on its own (delete, clone again) goes to M7-03.
- **Q140** Marked unreachable in these packages: `crypto/rand`, x509 and PKCS8 errors in `GenerateCerts`,
  `grpc.NewClient` (fails only on bad options), age and JSON encoding and `Sync`/`Close` after a good write in the
  credentials file, `ageKeys.Encrypt` (decrypting never calls it), `Chmod`/`Write` on a file just opened.
- **Q141** `workdir.Claim` ignores a marker it can't remove, said in a comment: it is this Application's, and the next
  Write deletes it.

## Agent, hub, engine (before M4-10c, 2026-11-17)

- **Q142** Engine failures where code calls Docker itself (`engine`; the Agent's `reclaim`, `DetectEngine`,
  `CheckPathParity`) are tested through a fault-injecting proxy in front of the real engine (`internal/enginetest`, a
  package only tests import, so `engine`, `agent` and later `importer` share it): a test-side `DOCKER_HOST`, not a hook in
  shipped code (Q96). Engine failures seen from the Agent stay on its fake engines (Q130).
- **Q143** The containerised Agent's restore after a reboot (Docker's placeholders reclaimed with the Agent's own image)
  is tested end to end in `agent-self.sh`: the app's files removed while the Agent is stopped, its container restarted
  by Docker, then the Agent started again.
- **Q144** `OutboxEntry.status_report` (3) is reserved: Status goes live since M4-02 and no Agent records one. An
  outbox entry of a kind the hub doesn't know is acknowledged and logged at Warn (an Agent is never newer than its
  Control Plane, Q41; unacknowledged it would be sent forever); a live envelope of a kind the hub or the Agent doesn't
  handle is logged at debug.
- **Q145** (not needed: a Sync's engine errors already reach its report and the Agent's "deploy failed" Error.)
- **Q146** `Engine.Status` skips a container gone between listing and inspecting it (Compose recreating a service),
  logged at debug, instead of failing: Self-Heal, status reports and Logs read it exactly then.

## Controller, api, cmd/omegacd (before M4-10d, 2026-11-17)

- **Q147** The server's, Source Server's and Agent's startup errors are tested end to end (`test/e2e/startup.sh`): the
  real binary started once per bad setting, exiting 1 with an Error line that says what to fix; `run`, `runSourceServer`
  and `runAgent` stay as they are.
- **Q148** A password over bcrypt's 72 bytes is the user's mistake, not the server's: changing to one is refused (400,
  "at most 72 bytes"), and `OMEGACD_ADMIN_PASSWORD` that long stops the server naming the setting and the limit.
- **Q149** The Agent's macOS warning is a function of the OS and the logger (`warnPlatform`), unit-tested with darwin
  and linux: the gate's Linux targets never run it.

## cli, importer, CI at 100% (before M4-10e, 2026-11-17)

- **Q150** The coverage total is over reachable code: marked statements leave it. CI's Coverage job, `make cover` and
  `make gate` run the check with `-all` alone (every reachable block covered is 100%), so `-base`, `-floor` and
  `test/coverage-floor` are deleted (this replaces Q133's floor of 100).

## UI (design check before M5, 2026-11-17)

- **Q151** Add Host (M5-01) shows the commands that work before M6: the `omegacd agent` command line `host add`
  prints, and a `docker run` of the published image with the same settings. M6-01 (`install.sh`), M6-02
  (`deploy/agent.yaml`) and M6-03 (systemd) each add their tab to the dialog; M6-03 none after all (Q247).
- **Q152** Hosts get a top-level page (cards, Add Host) next to Applications; Settings keeps credentials and API tokens.
- **Q153** The event stream carries a `host` event with the Host's view whenever it changes (enrollment, approval,
  connect and leave, the Agent's Hello, lock, unlock, rotation, allowlist, delete); the UI keeps Hosts in its live
  store like Applications.
- **Q154** A queued (Waiting) Sync Operation is cancelled with `POST /api/apps/{name}/syncs/{id}/cancel`: it finishes
  Superseded, "cancelled by admin" (Q81's phase); no new phase.
- **Q155** The Control Plane stores no logs: offline, the Logs tab says the Host is offline since when and that logs come
  from its Agent; the last known Live State, Health and Diff are shown as M4-02 and M4-11 keep them.
- **Q156** `source/v1` gains a `Status` RPC (version, the repo cache's size and Repository count, the blob cache's size),
  answered by the in-process Source Server too; an older Source Server's "unimplemented" shows as "version unknown".
- **Q157** The Agent's Hello gains `work_dir` and `work_dir_allow`: the Application form shows the Host's work dir as the
  placeholder and refuses a root outside the allowed ones with the Agent's own message; the Agent still enforces it.
- **Q158** M5's UI logic lives in pure `.ts` modules tested with `node --test`; the flows' API calls get the Go e2e;
  each PR shows screenshots of the screens it touched (light, dark, phone width). A browser test framework (Playwright)
  is reconsidered in M8 with UI coverage (Q135).
- **Q159** Add Host's `docker run` runs the Agent from the Control Plane's own image version (`:latest` for a development
  build) with the engine's socket, `/run/omegacd` mounted at the same path (Q21), its state in a named volume, and
  `OMEGACD_AGENT_ALLOW_ROOT=true`: root in a container holding the socket is root on the Host already.
- **Q160** Add Host's commands use an address field prefilled with the browser's address; opened over plain http on
  another address than localhost, the dialog warns that Agents connect over https only (Q95).

## Host cards (before M5-02, 2026-11-17)

- **Q161** The Hosts page shows one card per Host in a grid (one column on a phone), in place of M5-01's table.
- **Q162** A Host is renamed with `POST /api/settings/hosts/{name}/rename` and `omegacd host rename OLD NEW`, refused
  while an Application uses it (Q53). The Agent is found by its key, not its name; the rename drops its live
  connection, as the hub tracks connections by name, and the Agent reconnects under the new name.
- **Q163** "Agent outdated" (Q41) is decided by the Control Plane: `outdated` in the Agent's info when it is older than
  the Control Plane but still served; a development build is never outdated. `host list` shows it; M5-03's badge
  reuses it.
- **Q164** Revoke (delete) confirms in a dialog saying the Agent is cut off and enrolls again while its Applications
  keep running; Unlock confirms naming the two connections (Q75); Rotate key needs no confirmation and is enabled only
  while the Agent is connected.
- **Q165** The allowlist editor takes one address or CIDR per line, empty for any, validated by the server as
  `host allow` is; it shows the address the Agent last connected from, recorded with the Agent's info (`address`).
- **Q166** M5-02's e2e: `secrets.sh` encrypts with the `.sops.yaml` rule from `GET /api/settings/hosts/{name}/keys`
  (what the UI copies) and the secret deploys to that Host; `enrollment.sh` drives rename, rotate, unlock and the
  allowlist through the API.

## Trying M5-02 locally (2026-11-17)

- **Q167** A Sync of an App of Apps doesn't sync a Child Application that has `adopt: true` and was never synced: it
  is created and waits for its first manual Sync, after its Diff is checked (what `omegacd import` asks for). Until
  then every Sync of the App of Apps holds it back; afterwards it is synced like any other child. `autoSync: true` in
  its definition takes it over without review. Found when a Sync of the App of Apps took an imported stack over at
  once. In M6-05.
- **Q168** A **Date and time format** setting: Browser default (as before), European (`17.11.2026 18:42`, 24-hour) or
  US (`11/17/2026 6:42 PM`, 12-hour); every date and time the UI shows goes through it.
- **Q169** It is kept per browser (local storage), in a Display section of Settings: a viewer's preference, nothing on
  the server.
- **Q170** Q168 and Q169 are part of M5-02.
- **Q171** A Child Application is rolled back as any Application, as in Argo CD: an older Revision from its history,
  its Auto-Sync off. While it is rolled back, a Sync of its App of Apps neither syncs it nor turns its Auto-Sync back
  on (other settings from Git still apply); a manual Sync of the child ends the rollback. Its page says it is rolled
  back and why its App of Apps skips it. Replaces "Rollback is owned by Git" (v0.1's App of Apps), README included.
- **Q172** Q171 is its own ticket, M5-09, after M5-02; tested with Q167, as both change which children a Sync of an
  App of Apps syncs.

## Badges (before M5-03, 2026-11-17)

- **Q173** The UI works out the badges itself: each Application's Host joined with the Hosts store (loaded at start,
  kept by `host` events), and its last Sync Operation; no API change.
- **Q174** A rollback run on the Host (`omegacd agent rollback`, Q25) is recorded as the trigger `host-rollback` when
  its Agent reports it, told apart from a Rollback through the Control Plane (`rollback`); the protocol is unchanged.
  The badge "Rolled back on the Host" shows while the Application is rolled back by one.
- **Q175** The resource tree stays Application → services → containers: no Host node (tried, and dropped on review);
  the Application header right above it carries the Host's badges.
- **Q176** Engine (Docker or Podman, its version in the tooltip) shows on tiles, the Application header, list rows,
  the App of Apps' children and Host cards; Host offline, rolled back on the Host, queued Sync and
  Agent outdated only when they apply, each with a tooltip saying what to do. On Host cards the engine moves from its
  row into a badge.
- **Q177** M5-03 is the badges and their tooltips only; the offline views and cancelling a queued Sync are M5-04.
- **Q178** A Host card lists its Applications as links to them, each with a dot in its tile's color (green Synced and
  Healthy, yellow OutOfSync, red failing), under the label Applications: not status badges, which in OmegaCD always mean a status.
- **Q179** A container's uptime reads live: the Agent reports each container's start time (`State.StartedAt`, and
  `FinishedAt` for a stopped one) in the Live State (JSON, so no protocol change); the UI shows "Up 2 h 14 min" or
  "Exited (1) 5 min ago" counted from them, Docker's status line in the tooltip. An Agent that sends none shows the
  status line as before. Found in M5-03's review ("Up 3 seconds", frozen at the last report) and built in M5-03; M5-04
  adds "as of 14:05" while the Host is offline.
- **Q180** The Applications tiles view is one flat grid, as in Argo CD: an App of Apps is a tile like any other (with
  its child count), each of its children follows it as a normal tile naming it ("↳ platform", a link). No box around a
  family (a lone tile framed by a whole row when it had one child) and no section header (tried, and dropped on review).
  The list view keeps its tree with collapsing. An App of Apps' tile counts its children in its badge row, so its details
  are as many rows as any tile's, and its Host's badges are only those that need attention
  (Q181), so the grid's rows line up.
- **Q181** Where an Application names its Host (tile, list row, the App of Apps' children, the Application header) the
  Host's engine is text beside its name ("obleey-macbook · Docker 29.8.2"), not a badge: badges there only say what
  needs attention (Host offline, Agent outdated, rolled back on the Host, Sync queued), so they stand out. Host cards
  keep the engine badge. Changes Q176; the empty badge line Q180 kept goes with it.
- **Q182** A tile and a list row show a Repository by its name and path ("deploy · apps/blog"), the full URL in the
  tooltip and on the Application's page: a long URL cut off showed the part that says nothing.
- **Q183** The web UI moves from npm to pnpm, as its own ticket (M5-10): `pnpm-lock.yaml` in place of
  `package-lock.json`, pnpm from nix, and the Makefile, the Dockerfile, CI, the release workflow, CLAUDE.md and
  TESTING.md together; one package manager, never two lockfiles.

## Offline apps and queued Syncs (before M5-04, 2026-11-18)

- **Q184** An Application's view carries its Host's offline state as times, `hostOffline`, `hostOfflineSince` (when
  its Agent left) and `stateAt` (when what is shown was reported), not a sentence in `syncMessage` written in the
  server's time zone and format: the UI writes it in the viewer's chosen format (Q168), `omegacd app get` in local
  time. The UI also takes "offline" from the live Hosts, so a view not yet recomputed reads offline at once.
- **Q185** While its Host is offline an Application keeps its last known Health (Q38), dimmed, its tooltip "as of 14:05:
  Host … offline", beside the red "Host offline" badge; not Unknown as Argo CD shows.
- **Q186** A queued Sync is cancelled from the Last Sync card and its History row (no confirmation: nothing is undone)
  and with `omegacd app cancel NAME`; the store finishes it only while it still waits, so a Host coming back runs it.
- **Q187** Offline, Resources says "Last known, as of …: Host offline since …" above the tree, its services and
  containers dimmed, their uptimes "as of" the last report (Q179); the Logs tab says logs come only from the Agent.
- **Q188** The Source Server joins a Sync's log trail: the Control Plane passes the app and the Sync Operation's id with
  each call (gRPC metadata) and the Source Server's log lines carry them, so `op=…` reads through all three logs.
  In M5-05.

## Editing secrets (design check, 2026-11-18)

How people encrypt and edit the secrets a Host decrypts. A Host Key's private half never leaves its Host (Q108), so a
file encrypted for the Host alone can be opened by no person again.

- **Q189** OmegaCD keeps a list of **encryption keys**: a name and an age **public** key each, never a private one
  ("Miha's laptop: age1…"), in Settings → Encryption keys; not tied to accounts (v0.2 has one admin, Q49; v0.3's
  users can link to it). The admin adds keys in the UI or with `omegacd secrets key add NAME age1…`; a developer sends
  their public key.
- **Q190** Each key covers all Hosts (the default) or chosen ones. A Host's `.sops.yaml` rule gets its Host Key plus the
  keys allowed for that Host: the Host card's "Copy .sops.yaml rule" and "for all Hosts", `omegacd secrets init` and the
  import. A solo setup with one key on all Hosts needs no extra step.
- **Q191** Recovery is a key on the list kept offline (made with `omegacd keygen`, stored safely); the docs recommend
  one. With the list empty the UI warns that only the Hosts can decrypt: nobody can edit, and a lost Host loses them.
- **Q192** The Control Plane's Master Key is never a recipient: the Control Plane never decrypts an Application's
  secrets (Q108).
- **Q193** Removing a key leaves files as they are until `omegacd secrets rekey` re-encrypts every file for the current
  keys, on a machine holding one of them. Old commits stay readable to the removed key: the UI and docs say to change
  the secret values themselves when someone leaves.
- **Q194** `omegacd import` encrypts for the Host's key plus the people's keys allowed for that Host, not the Host's
  alone (whose files nobody could open again).
- **Q195** All of it is M6-08.
- **Q196** An Application warns, not fails, when its secret files are encrypted for another set of keys than its
  Host's key and the allowed people's: "run `omegacd secrets rekey`"; the Source Server compares each `*.sops.*`
  file's recipients as it renders. In M6-08, which builds the key list it compares against (M5-08's "this Host can't
  decrypt it" needs only the Host's key).
- **Q197** `omegacd secrets init` writes one rule per Application path (`path_regex: ^apps/shop/.*\.sops\.`, with that
  Host's keys), from OmegaCD's definitions; `--path` adds others. The Host card copies a per-Host rule for a quick start.
- **Q198** Taking a Host from a key is a removal (Q193): rekey, and change that Host's secret values.
- **Q199** Any logged-in user or token reads the key list (public halves; `GET /api/settings/encryption-keys`, what
  `secrets init` and `rekey` use); only the admin changes it.
- **Q200** `omegacd secrets rekey` goes through every file, rekeys what it can and lists the ones it can't open
  (encrypted only for keys the user doesn't hold, such as a Host's alone): write their values again and encrypt them.
  Q196's warning names the same files.
- **Q201** `omegacd secrets init` and `import` refuse to encrypt for a Host alone when no person's key covers it ("add
  yours, or pass `--host-only`"); the Host card warns beside "Copy .sops.yaml rule". Only a hand-written rule can still
  make such a file, and Q196's warning shows it.

Two things the docs say with it: the key list controls who can **read** a Host's secrets, not who can **change**
them: anyone who can push to the repository can encrypt a new value with the Host's public key, so who may change
production is decided in Git (branch protection, reviews). And `omegacd keygen` writes a person's key where `sops`
looks for it by default (Q290: `~/.config/sops/age/keys.txt` on Linux, `~/Library/Application Support` on macOS).

## Source Server settings (before M5-05, 2026-11-18)

- **Q202** Settings → Source Server, above the credentials: in-process or standalone at its address, reachable or why
  not with how long it took, its version, the Repository cache (Repositories and size) and the blob cache's size;
  Refresh reloads it. From `GET /api/settings/source` and the `Status` RPC (Q156).
- **Q203** A Source Server of another version than the Control Plane's minor or one behind (Q41's rule) is warned in that
  section, not refused: Syncs keep working while the calls succeed.
- **Q204** The Control Plane sends the app and Sync Operation its context carries (`omegacd-app`, `omegacd-op` gRPC
  metadata) with every call; the standalone Source Server puts them on its log lines (Q188). In-process the context
  flows in as is. The e2e follows one Sync's `op=` through all three logs.
- **Q205** Proof that the Control Plane stores no Repository secret: the e2e adds, rotates and deletes a credential
  through a standalone Source Server and finds neither token in the Control Plane's data; and with Forgejo running,
  the standalone Source Server reads a private Forgejo repository with a credential the Control Plane never holds.
- **Q206** No cache button: Settings shows the caches' size, the Source Server manages them.
- **Q207** A Host card lists its Applications grouped by the App of Apps that declares them, the standalone ones after:
  the App of Apps is a pill like its children (its icon, its status dot, a link), heading its group; it deploys
  nothing itself, the group says its children run there.

## Secret files by their key names (before M5-06, 2026-11-18)

- **Q208** A decrypted file shows its key names, `KEY=••••`, read by the Control Plane from its encrypted source in Git
  (`app.sops.env` for `app.env`): SOPS encrypts values only, so the names are readable without decrypting anything.
  The Agent reports nothing about secret files, so no code path can carry a decrypted value from it (the ticket's
  "from the key names the Agent reports" is dropped).
- **Q209** dotenv and INI read `KEY=••••` in file order; YAML and JSON `db.password: ••••`, nested keys as paths and
  list items by index; SOPS's own metadata is never shown; a binary SOPS file shows "none: an encrypted binary file".
- **Q210** No value in any call: a unit test shows a file encrypted with known values as its key names only, neither
  value nor ciphertext in the view; the e2e checks `API_TOKEN=••••` in the API and that no secret value is in the API,
  the Control Plane's data or its logs.
## Rollback of a Child Application (before M5-09, 2026-11-19)

First written as Q208–Q210 on `main`; renumbered Q220–Q222, as M5-06 to M5-08 had taken Q208–Q219 on their branches.

- **Q220** An App of Apps shows a rolled-back child: a "Rolled back" badge on the child in its children list, and on
  the App of Apps' page how many children are rolled back and that its Syncs skip them. A parent that looks Synced
  while it skips a child would hide that Git is not what runs. Worked out in the UI from the children's `rolledBack`
  (Q173), no API change.
- **Q221** A manual Sync of a rolled-back child ends the rollback as for any Application: the flag goes, Auto-Sync
  stays off. The next Sync of its App of Apps applies the child's settings from Git again, `autoSync` included; until
  then nothing syncs it automatically.
- **Q222** A rolled-back child removed from Git is treated like any other: with Prune on its App of Apps it is deleted,
  without it is marked not in Git (as `main` already does). Removing it from Git is the later, deliberate act, as
  in Q81. M5-09 tests it.

## Work dir in the Application form (before M5-07, 2026-11-20)

- **Q211** The Agent's Hello carries its work dir and the further roots it allows (Q157); the Control Plane checks an
  Application's `workDir` against them when saving (create, edit, an App of Apps' child) and refuses with the Agent's
  own words; the form shows it as you type. Roots not reported yet (never connected, an older Agent): accepted, the
  Agent decides at the Sync. The Agent always enforces it; both use `workdir.Allowed`.
- **Q212** The form's Work dir field has the Host's work dir as its placeholder and its allowed roots as the hint; an
  Application's page and a Host card show the work dir and the roots.
- **Q213** `omegacd app create --work-dir PATH`, matching `workDir:` in a definition and the form.
- **Q214** Changing an Application's work dir is allowed: the next Sync deploys into the new folder, the old one and its
  data stay (the Agent moves nothing); the form warns to move them by hand first.

## "This Host can't decrypt it" (before M5-08, 2026-11-20)

- **Q215** A secret source's reason is the failed Sync Operation's message, the field the UI, History and
  `app sync --wait` already show; a later source reports its own reason there too. For SOPS: which file, the keys
  it's encrypted for, this Host's key, and the fix (add the key to the file's rule in `.sops.yaml`, `sops updatekeys`).
- **Q216** This Host's key is in full, to paste; the file's keys are shortened (`age1abc…7x2c`), to recognize. Public
  keys and a file name only: never a secret.
- **Q217** No new retry rule: Auto-Sync's backoff gives up on that Revision; the next commit gets a fresh try.
- **Q218** The Agent no longer reads decrypted files for their key names (`Decrypted.Keys`, unused since Q208).
- **Q219** Unit: SOPS with another key's file, and a second source's own reason, read the same way; e2e: the message
  in `app sync --wait` and the API's history, with no secret.

## Rollback of a Child Application, building it (before M5-09, 2026-11-20)

- **Q223** Q167 (a child adopting a running stack waits for its first manual Sync) is built in M5-09 with Q171: the
  same check decides which children a Sync of an App of Apps leaves alone. M6-05 no longer carries it.
- **Q224** A "Rolled back" badge wherever a rolled-back Application shows (tile, row, children, its page), its tooltip
  the Revision and the way out; for a child, that its App of Apps skips it until you Sync it. "Rolled back on the Host"
  stays its own (Q174).
- **Q225** A rolled-back child's banner says its App of Apps leaves it alone and that a Sync of it ends the rollback,
  after which its App of Apps applies Git's settings again; standalone Applications keep "turn Auto-Sync back on".

## pnpm for the web UI (before M5-10, 2026-11-20)

- **Q226** Node 26 ships no corepack, so pnpm's version is pinned once, in `web/package.json`'s `packageManager`
  (`pnpm@12.21.0`): CI and the release workflow install it with `pnpm/action-setup` (pinned by SHA), the Dockerfile
  with `npm install -g` of that same field, and a local pnpm switches itself to it. Replaces "a pinned corepack" (Q183).
- **Q227** pnpm comes from nix's common package list, beside `nodejs`, on every machine.
- **Q228** M5-10 skips the Linux gate: no Go code changes and the built UI is the same (identical but for SvelteKit's
  build stamp); the PR shows the comparison, the image built locally, and the UI's check and tests under pnpm.

## Install and import: design check (before M6, 2026-11-20)

- **Q229** M6 is worked in the order its tickets build on each other: M6-02 (compose files), M6-03 (systemd units),
  M6-09 (.deb), M6-01 (`install.sh`, which installs what those three make), M6-04 (Kubernetes), M6-06 (reverse
  proxies), M6-08 (secrets helper and the key list), M6-05 (import, which encrypts for that list). Ticket numbers stay.
- **Q230** The import runs on the Agent: the CLI sends the recipients (the Host's key and the people's keys allowed
  for it, Q194) and the Application names already in the checkout; the Agent reads the stacks, runs the importer and
  returns the finished files with every secret already SOPS-encrypted; the Control Plane relays only that, never a
  plaintext secret (its trust boundary). The CLI writes the files into the checkout: the overwrite check, `--force`
  and `--dry-run` stay its own.
- **Q231** `deploy/compose.yaml` runs the Source Server standalone by default, as Kubernetes does: two services, the
  mTLS certificates made at first start by a one-shot `omegacd source-certs` into a shared volume, no manual step.
  In-process stays for `omegacd server` alone and for development.
- **Q232** `install.sh agent` installs the `.deb` where apt is there (the system unit with a docker-group user; the
  package's user unit with lingering when it finds a rootless engine), and the container (`deploy/agent.yaml`) when
  asked or without apt; always with recommended packages (M6-01's Debian note).
- **Q233** `install.sh full` keeps both targets, compose and kubernetes (the context always confirmed; kubernetes
  dropped for v0.2 by Q276), and drops
  v0.1's leftovers: no `age-keygen` step (the Control Plane makes its Master Key, Q115), no `known_hosts` file (Git
  over SSH only, optional, Q106), no "Master Key for .sops.yaml" line (never a recipient, Q192); it ends with the
  next step: open the UI and add a Host.
- **Q234** `install.sh` installs the version it was released with: the release workflow attaches it to each GitHub
  Release with that version filled in, and the README's one-liner fetches it from the release (pre-releases, such as
  M7-04's `v0.2.0-rc.N`, too); there is no `latest` image before v0.2.0. Tests point it at local builds (a binary, a
  `.deb`, an image), so no M6 ticket needs a release.

## Compose files (before M6-02, 2026-11-20)

- **Q235** A containerised Agent finds its own container by the ID in `/proc/self/mountinfo` (Docker and Podman
  mount its `hostname` and `hosts` files from a path holding it), so host networking works; the hostname stays the
  fallback. The path-parity check, the Q59 reclaim and the Agent's own container all use it.
- **Q236** The image runs as a fixed user `omegacd` (uid and gid 10001) and `/data` is its own, so a new named volume
  inherits that. `deploy/compose.yaml` mounts no socket; `deploy/agent.yaml` runs as `omegacd` with
  `group_add: ["${DOCKER_GID}"]` from a `.env` beside it. Its rootless variant: Q240.
- **Q237** A containerised Agent whose work dir isn't writable (`/run` is emptied at boot, and Docker recreates a
  missing bind source owned by root) gives the folder to its own user at start, through Docker, with a short-lived
  container of its own image, as the Q59 reclaim does: no `mkdir` or `chown` for the operator, after a reboot either.
- **Q238** `deploy/agent.yaml` uses host networking, so an Agent beside its Control Plane reaches it at
  `http://127.0.0.1:8080` (loopback, Q95); a remote one takes its `https://` URL. Add Host's tab shows the file with
  the invite and URL filled in, and `stat -c %g /var/run/docker.sock` for the GID a browser can't know.
- **Q239** An e2e, `deploy-compose.sh`, builds the image with coverage, brings up `deploy/compose.yaml` (Control
  Plane and standalone Source Server, both non-root) and `deploy/agent.yaml` against it, enrolls, deploys an
  Application with a bind-mounted file, and checks the refusal of a work dir mounted at another path; on the Linux test
  servers too, rootful and rootless Docker and rootless Podman.
- **Q240** A rootless engine's container root is the unprivileged user on the host (a container uid 1000 maps to a
  subordinate uid that can't open the user's socket), so the rootless variant of `deploy/agent.yaml` runs as `0:0`:
  the Agent accepts euid 0 without `OMEGACD_AGENT_ALLOW_ROOT` when it is in a container and its engine reports
  rootless; everywhere else root is still refused. One file: the rootless variant is `.env` values (user `0:0`, the
  user's socket, a work dir under `/run/user/<uid>`). Corrects Q236's first answer, `user: "${UID}:${GID}"`.
- **Q241** (while building M6-02) The test servers have rootless Docker only inside a dind container, whose
  RootlessKit network keeps an Agent container from the Control Plane's loopback; so M6-02 proves the rootless variant
  of `deploy/agent.yaml` on the servers' native rootless Podman (the Agent's check is the engine's rootless flag, the
  same for both), and rootless Docker comes with M6-03: native rootless Docker for the test user on the servers
  (omegacd-testinfra), which its rootless user unit needs anyway, and `deploy-compose.sh`'s rootless Docker variant.

## systemd units (before M6-03, 2026-11-21)

- **Q242** The units live in `packaging/`: `omegacd-agent.service` and `omegacd-agent-restore.service` for the system,
  the same two names under `packaging/user/` for a user. The system unit runs as a system user `omegacd-agent` in the
  `docker` group, apart from the Control Plane's `omegacd` (the docker group is root's equal, which the Control Plane
  needs not be), its settings in `/etc/omegacd/agent.env` (0640 root:omegacd-agent: the Control Plane's URL and the
  invite); the user unit's in `~/.config/omegacd/agent.env`. `ExecStart=/usr/bin/omegacd agent`, where M6-09's `.deb`
  puts it: one install path (Q232), and the README's manual steps put it there too.
- **Q243** The work dir is the units' `RuntimeDirectory=omegacd` with `RuntimeDirectoryPreserve=yes`: systemd removes
  a runtime directory when its unit stops, which would take the deployed files from under running containers at every
  Agent restart or upgrade; a reboot still empties it (`/run` is a tmpfs), so plaintext stays in RAM (Q59).
- **Q244** The user unit has its restore unit too, ordered `Before=docker.service podman-restart.service` in the
  user's scope: rootless Docker's user `docker.service` (with lingering) and rootless Podman's `podman-restart.service`
  restart containers at boot before the Agent runs, Q59's case. Each restore unit is `WantedBy=` its engine's units, so
  it runs whenever the engine starts, not only at boot.
- **Q245** The system unit is hardened as the Control Plane's (`NoNewPrivileges`, `ProtectSystem=full`, the kernel
  protections, `RestrictSUIDSGID`, `LockPersonality`) but without `PrivateTmp` or `ProtectHome`: they would show the
  Agent another `/tmp` or `/home` than the one Docker resolves bind mounts in, breaking path parity (Q21) for work dirs
  allowed there (Q57).
- **Q246** The system unit is for Docker (rootful Podman needs root, not a group); Podman Hosts use the user unit. For
  them `packaging/containers.conf.d/omegacd-agent.conf` sets `events_logger = "file"`, `log_driver = "k8s-file"` and
  `log_size_max` of 10 MiB; the README says where it goes until the `.deb` (M6-09) and `install.sh` (M6-01) install it.
- **Q247** M6-03 adds no tab to Add Host (amends Q151 for it): without a package a systemd tab would be some fifteen
  manual lines, which M6-01's `install.sh` tab replaces two tickets later, installing the `.deb` with these units. Until
  then the README has the manual steps; Add Host keeps Docker Compose and the command line.
- **Q248** An e2e, `systemd.sh`, installs the units from `packaging/` with the built binary, deploys an app with a
  restart policy and a decrypted file it mounts, and reboots in simulation: the Agent and the engine stopped, the work
  dir emptied, the engine started; the restore unit must run first, so the container comes back with its file, not a
  root-owned folder; then the Agent reconnects. System unit on rootful Docker, user unit on the test user's native
  rootless Docker and on rootless Podman. The system unit needs root: the test user has passwordless `sudo` on the
  test servers (Ansible), as CI's runners have; macOS skips it, and on Linux under `test-all` a missing systemd or sudo
  fails.
- **Q249** And a real reboot in the gate: after the suite `test/linux.sh` sets up the system unit and a user unit with
  an app each, reboots the server, waits for SSH and checks both apps run with their files: only a boot proves the
  order against `docker.service` starting with the machine. Some two minutes a server; CI can't reboot and has Q248's
  simulation.
- **Q250** Native rootless Docker for the test user on the test servers (omegacd-testinfra, Q241): `rootlesskit`, and
  `dockerd-rootless.sh` from Debian's `docker.io` or, on Ubuntu, whose package lacks it, moby's `contrib/` at the
  installed Docker's tag; a user `docker.service` with lingering, on `/run/user/1000/docker.sock`. `deploy-compose.sh`
  gains its rootless Docker variant there.
- **Q251** "Binary + systemd" drops the Control Plane's v0.1 leftovers, as Q233 does for `install.sh`: the unit's
  `OMEGACD_SSH_KNOWN_HOSTS` line (the default anyway; the file is optional, Git over SSH only, Q106), the README's
  `known_hosts` step and its `v0.1.0` download.
- **Q252** (while building M6-03) Rootless Docker's host network is RootlessKit's namespace, whose loopback isn't the
  Host's: a container Agent there can't reach a Control Plane on the same machine at `http://127.0.0.1` (Q238 holds for
  rootful Docker and Podman). It takes the Control Plane's `https://` URL, as a remote one does, and the Agent keeps
  refusing plain http beyond loopback (Q95); beside its Control Plane on rootless Docker the user unit fits.
  `deploy-compose.sh`'s rootless Docker variant connects through a TLS front on the server's own address, its private
  CA in `OMEGACD_AGENT_CA_FILE`.

## .deb packages (before M6-09, 2026-11-21)

- **Q253** One package, `omegacd`: the binary, the Agent's system units, the user units (`/usr/lib/systemd/user/`)
  and the Control Plane's `omegacd.service`, which the package never enables. "Binary + systemd" becomes installing
  the `.deb` and `systemctl enable --now omegacd`: one install path for both (Q232).
- **Q254** `/etc/omegacd` is root's, 0755, made by the package; the Agent's `agent.env` stays in it (Q242), and the
  Control Plane's Master Key moves to `/etc/omegacd/keys/age.key`, a folder of its own (0700, `omegacd`'s), which its
  unit may write (`ReadWritePaths`; `ProtectSystem=full` makes `/etc` read-only). Before, the Control Plane's steps
  made `/etc/omegacd` 0750 `omegacd`'s and the Agent's 0755: on one machine the second lost.
- **Q255** Installing creates both users from a `sysusers.d` file (`omegacd-agent` in `docker`, the group created
  when Docker isn't there yet) and enables the Agent's system units, as Debian enables a package's services
  (`deb-systemd-helper`, which keeps an admin's disable); `omegacd-agent.service` gains
  `ConditionPathExists=/etc/omegacd/agent.env`, so a machine without it (a Podman Host, a Control Plane alone) skips it
  at boot. The Agent starts at install only when `agent.env` is there. The Control Plane's unit and the user units are
  never enabled by the package (`--global` would start an Agent for every user); Podman's drop-in goes to
  `/usr/share/omegacd/containers.conf.d/`, copied by the operator or `install.sh`, not into `/etc/containers`, which
  would change Podman for every user.
- **Q256** An upgrade restarts what runs (`systemctl try-restart` of `omegacd` and `omegacd-agent`; the work dir and
  the apps stay, Q243) and the running user Agents of lingering users (`systemctl --user -M USER@`), which would
  otherwise run the old binary until a reboot.
- **Q257** `apt remove` stops the units and keeps the users, `/etc/omegacd` and both state dirs. `apt purge` also
  removes the Agent's state (`/var/lib/omegacd-agent`, its keys: an Agent is enrolled again), `agent.env`,
  `/run/omegacd` and both users (amended by Q261: not the users); never the Control Plane's data or its Master Key (losing it loses every sealed
  credential), which it says it kept; never users' homes (the user units' state). Removing stops running user Agents
  too: their binary is gone.
- **Q258** nfpm builds it, through `go run …/nfpm@v2.48.0` in `make deb` (as buf runs): the release workflow and the
  tests use the same, so nothing is added to nix or the test servers. The binary is static: no `Depends`.
- **Q259** The release's packages are named like its archives, `omegacd_v0.2.0-rc.1_linux_amd64.deb`, and listed in
  `SHA256SUMS`; the version inside is Debian's `0.2.0~rc.1`, so a release sorts after its release candidates.
- **Q260** `systemd.sh` installs this checkout as a `.deb` (the coverage binary inside), not copied units: the system
  unit and the user units as the package ships them, so the gate's real reboot (Q249) proves the package; it adds an
  upgrade (a higher version: the Agents restart on it with the same key while the app's container runs on), `apt
  remove` and `apt purge`, and checks the Control Plane's unit. The README's systemd steps become the `.deb`.
- **Q261** (while building M6-09; amends Q257) `apt purge` deletes neither user, as Debian's packages leave their
  system users: it keeps the Control Plane's data and Master Key, owned by `omegacd`, and the Agent may have written
  files outside the folders purge removes (a work dir allowed on disk), so a deleted user's uid, given to the next
  system user, would own them; and a user of that name may not be the package's (an admin's `useradd`, or the test
  servers' own login `omegacd`, which `sysusers` keeps). Purge says it kept them; a reinstall reuses them.

## install.sh (before M6-01, 2026-11-22)

- **Q262** (kubernetes deferred by Q276) `install.sh full` drops the age-keygen step for compose only: `deploy/kubernetes.yaml` still mounts v0.1's
  keys Secret read-only, so its target keeps making the Secret until M6-04 changes the manifest and drops the step,
  tested in OrbStack's Kubernetes there. M6-01 tests compose and agent mode.
- **Q263** Settings for unattended runs: `OMEGACD_MODE` (`full`, `agent`); full: `OMEGACD_TARGET` (`compose`,
  `kubernetes`; gone with Q276) and today's; agent: the Agent's own `OMEGACD_AGENT_CONTROL_PLANE` and `OMEGACD_AGENT_INVITE`,
  `OMEGACD_AGENT_INSTALL` (`systemd`, the default with apt; `container`, Q232) and `OMEGACD_AGENT_ENGINE` (`docker`,
  `rootless-docker`, `podman`; unset: the first found in Q103's order, asked with it as default when several are).
  Whatever is missing is asked on the terminal.
- **Q264** It runs as the user whose engine the Agent uses, with `sudo` only for what needs root (`apt install`,
  `/etc/omegacd/agent.env`, lingering when not allowed without); as root it offers rootful Docker only.
- **Q265** Agent mode installs the release's `.deb`. Rootful Docker: `/etc/omegacd/agent.env` (0640
  `root:omegacd-agent`), `omegacd-agent` started. Rootless Docker or Podman: `~/.config/omegacd/agent.env` (0600,
  `DOCKER_HOST` for Podman), lingering, the user's `omegacd-agent-restore` enabled and `omegacd-agent` started; Podman
  also the package's drop-in copied, `podman.socket` restarted and `podman-restart.service` enabled, so containers with
  `restart: always` come back at boot with their files (Q59). Container: `agent.yaml` and `.env` in
  `/opt/omegacd-agent` (`~/omegacd-agent` on a rootless engine) with Q240's rootless values; on rootless Docker it
  refuses an `http://127.0.0.1` Control Plane (Q252), naming the user unit. Each ends printing the fingerprint to
  approve.
- **Q266** The release workflow attaches `install.sh` with `@VERSION@` filled in, listed in `SHA256SUMS` (Q234); it
  fetches the `.deb`, `compose.yaml` and `agent.yaml` of that version and checks the `.deb` against `SHA256SUMS`.
  `OMEGACD_DOWNLOAD` (`file://` too) and `OMEGACD_IMAGE` point it at local builds for tests. From `main`, unfilled, it
  stops: use the release's `install.sh`, or set `OMEGACD_VERSION`. The README's one-liner is
  `…/releases/latest/download/install.sh`.
- **Q267** Running it again upgrades: the newer `.deb` (which restarts the Agent, Q256), the container's new image in
  `.env` and `up -d`, compose's new image; `agent.env` rewritten only from values given (an enrolled Agent ignores an
  invite); the Master Key never touched.
- **Q268** Add Host gets an **install.sh** tab, first and the default, with the release's one-liner and the invite
  filled in; a development build's tab says it needs a release and points at Docker Compose and Command line, which
  stay.
- **Q269** The tests drive the existing scripts through `install.sh`, as Q260 did for the package: `systemd.sh`
  installs the system and both user units with `install.sh agent` from a local `.deb` (lingering, Podman's drop-in and
  `podman-restart` included; coverage drop-ins placed first); `deploy-compose.sh` brings up the Control Plane with
  `install.sh full` (the coverage image in `OMEGACD_IMAGE`, its overrides in `COMPOSE_FILE`) and the Agent with
  `install.sh agent` in a container, rootful and rootless; plus a rerun upgrading with the state kept, and `main`'s
  copy refusing. The gate's real reboot covers what it installed.

## Kubernetes (before M6-04, 2026-11-22; deferred by Q276)

The design M6-04 was built to, kept for when it comes back (branch `park/m6-04-kubernetes`, tried on OrbStack only):

- **Q270** The Source Server's mTLS certificates are made at install, once, into a Secret `omegacd-source-tls`:
  `install.sh` makes them in a pod of the image (`omegacd source-certs --host omegacd-source-server`, read from its
  log once it ends: `kubectl run -i` loses the output of a pod that ends before it attaches), by hand
  `omegacd source-certs`. Each pod mounts only its side's three files. No Job, RBAC or Kubernetes API code.
- **Q271** The Master Key on its own small PVC `omegacd-keys` at `/etc/omegacd`, made at the first start (Q115), apart
  from the data PVC, as Compose's `omegacd-keys` volume.
- **Q272** `known_hosts` an optional Secret `omegacd-known-hosts`, mounted in the Source Server's pod only (Q106); the
  Source Server has its own PVC for `source.key` and the clones.
- **Q273** The manifest's image is `ghcr.io/obleey/omegacd:latest`, as `compose.yaml`'s; `install.sh` puts its
  release's in.
- **Q274** Tested by `test/e2e/kubernetes.sh` in `make e2e`: OrbStack's cluster on macOS, a kind cluster (`go run`,
  pinned; kubectl fetched and checked when missing) on the Linux servers and CI; `install.sh` full into a namespace of
  its own, a remote Agent enrolled through a port-forward deploying an Application from Forgejo, and a rerun on
  another image keeping the Master Key and the certificates. A Service routes to a new pod seconds after it's ready,
  so the test retries its Syncs as the Control Plane does (Q124).
- **Q275** No NetworkPolicy (mTLS keeps others out of the Source Server), no Helm chart; the commented Ingress has
  TLS: remote Agents take `http://` only on loopback (Q95).
- **Q276** Kubernetes is deferred until after v0.2, as the RHEL family is (Q61): OmegaCD is for Docker hosts, and teams
  on Kubernetes deploy with Argo CD or Flux. v0.2 drops `deploy/kubernetes.yaml`, `install.sh`'s `kubernetes` target
  (`OMEGACD_TARGET` with it: full mode is Compose) and the README's Kubernetes option; M6-04 waits in the ROADMAP.

## Reverse proxies (before M6-06, 2026-11-22)

- **Q277** The examples are files, tested as shipped: `deploy/proxy/` holds one Compose override per proxy
  (`caddy.yaml` with its `Caddyfile`, `traefik.yaml`, `nginx.yaml` with its `nginx.conf`), used as
  `docker compose -f compose.yaml -f proxy/<name>.yaml up -d`; the README's "Reverse proxy" section links them.
- **Q278** OmegaCD trusts `X-Forwarded-For` from the proxy's address alone: each override puts the proxy and the
  Control Plane on a network of their own, both at fixed addresses (proxy `.2`, Control Plane `.3` of
  `OMEGACD_PROXY_NET`, default `172.30.255`, a `/29`), so neither the network's gateway (Docker's own relays of
  loopback traffic) nor any other container is trusted, and no address is handed out twice. nginx replaces the header
  instead of appending to it, as Caddy and Traefik do. The Control Plane's port stays published on `127.0.0.1` only,
  for an Agent on the same machine (Q238). A proxy the operator already runs: trust its address or its network,
  knowing every container on that network is then trusted.
- **Q279** TLS: Caddy's automatic HTTPS (`OMEGACD_DOMAIN`), Traefik's Let's Encrypt resolver over the HTTP challenge
  (`OMEGACD_ACME_EMAIL`), nginx with a certificate in `OMEGACD_TLS_DIR` (certbot's names). nginx asks Docker's DNS
  again every 10 s, so a recreated Control Plane is found; its body limit is 5 MB, a webhook's.
- **Q280** `test/e2e/proxies.sh` runs the three side by side in front of the Compose Control Plane: https with http
  redirected, a Secure cookie, an API token, an Agent over `wss://` trusting the proxy's certificate
  (`OMEGACD_AGENT_CA_FILE`), the client's address from the proxy's header only (a client's own one changing nothing,
  `host allow` keeping and dropping the Agent by it), 75 s of quiet past nginx's 60 s idle timeout, and a recreated
  Control Plane. Certificates: Caddy's own for `localhost`, a test CA's for Traefik and nginx.
- **Q281** A proxy in front of a standalone Source Server: the README's TLS-passthrough note stays as it is, no
  example (`compose.yaml` keeps the two on one network).
- **Q282** `install.sh` offers no proxy; its closing line points at `deploy/proxy` and the README section.
- **Q283** Cloudflare needs nothing of OmegaCD: in front of the operator's proxy it passes WebSockets and API tokens;
  the README says how to restore the real client address (Cloudflare's ranges trusted by the proxy, or in
  `OMEGACD_TRUSTED_PROXIES`). No Tunnel example and no Cloudflare-specific configuration.
- **Q284** Cloudflare is tried by hand once, in M7-04's real-world run (proxied DNS in front of the proxy), not in
  the e2e suite: that would need an account, a domain and a token on every run.
- **Q285** With Cloudflare Access, the README says to keep it to the UI: a Bypass rule for the Agents'
  `/api/agents/`, `/api/webhooks/` and API-token calls, which can't log in with a browser, or a service token for the
  CLI.

## Secrets helper (before M6-08, 2026-11-22)

Building Q189–Q201:

- **Q286** `omegacd secrets init --host NAME` works in a checkout: the repository is the origin remote (`--repo-url`
  overrides), and it writes one rule per Application of that Host whose `repoURL` is that repository (compared without
  scheme, user and `.git`); an App of Apps gets none (its path holds definitions); `--path P` adds `^P/.*\.sops\.`.
- **Q287** In an existing `.sops.yaml` OmegaCD's rules replace those with the same `path_regex`, and new ones go first
  (SOPS takes the first match, so a catch-all at the end keeps working); other rules and comments stay.
- **Q288** `omegacd secrets rekey` needs no `sops` binary: the SOPS library, as `sops updatekeys` (same data key and
  values), with the person's key read where `sops` reads it (`SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE`, its keys file).
  It first refreshes OmegaCD's rules in `.sops.yaml` from the key list, then gives every `*.sops.*` file its rule's
  recipients; files no rule matches are listed and left.
- **Q289** A Host Key changes only when its Host enrolls again with a new state dir (reinstall, lost `host.key`);
  `rekey` with a person's key makes the files deployable again. No command rotates a Host Key. A Host with
  Applications gets there as Q127 says: its Applications forgotten (`app delete --forget`), the Host deleted and added
  again, the Applications created again; deleting it takes it off the keys that named it (Q291).
- **Q290** `omegacd keygen` writes by default where `sops` reads (corrects Q201's path): `$XDG_CONFIG_HOME/sops/age/keys.txt`,
  else the OS's config dir (`~/.config` on Linux, `~/Library/Application Support` on macOS). A file there with a key
  already: no new one, its public key printed (`--out FILE` for another, `--out -` stdout).
- **Q291** The key list: a table of keys (name, public key, all Hosts or not) and one of chosen Hosts per key, renamed
  and deleted with the Host; a key whose chosen Hosts are all deleted covers none, never all. Adding refuses the Master
  Key (Q192) and any Host's Host Key (rules get those already).
- **Q292** CLI: `omegacd secrets key add NAME age1… [--host H]...` (no `--host`: all Hosts; an existing name: changed),
  `secrets key list`, `secrets key remove NAME` (printing Q193's advice). UI: Settings → Encryption keys: list, add,
  edit, delete with the same advice, and Q191's warning when the list is empty.
- **Q293** Q196's warning: the Control Plane sends the expected recipients in the render request (a new optional proto
  field); the Source Server compares each `*.sops.*` file's recipients with them exactly: a missing key and a removed
  one both warn, naming the file and "run `omegacd secrets rekey`". None while the Host has no Host Key. A CI key
  that reads them belongs on the list.
- **Q294** `omegacd import` encrypts for the Host Key plus the keys allowed for that Host, refusing a Host-only set
  without `--host-only` (Q194, Q201); `--recipient` still replaces the set. M6-05 moves it to the Agent.
- **Q295** One small interface per secret source (`Init` writes rules, `Rekey` rekeys); SOPS ships, a dummy second one
  lives in tests; `--source` takes `sops` only in v0.2.
- **Q296** The Host card's "Copy .sops.yaml rule" and "for all Hosts" add the keys allowed for each Host, with Q201's
  warning when none covers it; the copied rule keeps its `apps/<host>/` example path.
- **Q297** `rekey` rekeys what it can, lists the files it can't open with the fix, and exits 1 when there are any.

## Initial admin password (before M6-11, 2026-11-22)

- **Q298** `OMEGACD_ADMIN_PASSWORD` goes: the admin's first password is always generated, never configured, and the
  first login must change it. A password in `.env` sat in plain text and outlived the first start without ever being
  changed.
- **Q299** Its own ticket, M6-11 (issue #161), right after M6-08.
- **Q300** The generated password is written once to `initial-admin-password` in the data dir (mode 0600) and the
  file is deleted when the password is changed; the log says only where it is, so Q46's one logged secret goes.
  `install.sh full` prints it at the end; by hand `docker compose exec omegacd cat /data/initial-admin-password`.
- **Q301** All of `OMEGACD_ADMIN_PASSWORD`'s code goes, with no warning: an install still setting it simply has it
  ignored, and an admin who exists keeps their password.
- **Q302** One shared e2e helper reads the file, logs in, changes the password and makes the API token; every e2e
  script, `deploy-compose.sh` and the README walkthrough use it.
- **Q311** `omegacd reset-admin-password`, run on the Control Plane's machine with its data dir (Compose:
  `docker compose exec omegacd omegacd reset-admin-password`; `.deb`: `sudo omegacd-reset-admin-password`, Q316), generates a
  new password into `initial-admin-password`, requires it changed at the next login and ends every session; API tokens
  keep working. A lost password has a way back.
- **Q312** While the file exists, every start logs at warn where it is ("log in as admin and change it"), never the
  password; after the change, nothing.
- **Q313** The file is never in `omegacd backup`; a restart before the first login keeps it and its password; if it
  can't be deleted after the change, the change still counts and the error names the path to delete by hand; an install
  whose generated password is only in its old log gets no file on upgrade (the log has it; Q311 covers the rest).
- **Q314** The login page shows a fixed hint where the first password is; no unauthenticated "setup pending" endpoint.
- **Q315** API tokens need no check of their own: one can be made only after logging in, which the must-change block
  already stops until the password is changed.
- **Q316** (corrects Q311's `.deb` line) Run from a shell, `omegacd reset-admin-password` has neither the unit's
  settings nor its user: as root it would leave the database's `-wal`/`-shm` files and the password file root's. The
  `.deb` ships `omegacd-reset-admin-password`, a few lines of shell running it with the unit's settings as `omegacd`
  (`runuser`): `sudo omegacd-reset-admin-password`. The command itself refuses root on a database another user owns,
  naming that wrapper.

## Import through the Agent (M6-05, 2026-11-23)

- **Q317** The importer splits where Q230 draws the line: the Agent's half lists the stacks, reads their files, decides
  config, data and secrets, rewrites the compose files and encrypts, and returns per stack its files, definition and
  report; the CLI's half checks for an existing `apps/<name>`, applies `--force` and `--dry-run`, writes the files and
  appends the definitions. The CLI sends the names already defined, so the Agent skips those stacks.
- **Q318** The CLI calls `POST /api/settings/hosts/{name}/import` (beside the other Host actions); the Control Plane relays an `ImportRequest` to that Host's
  Agent and waits for its answer, as `Diff` does; an offline or unapproved Host gets the usual error. `--timeout`
  (default 5m) bounds the wait.
- **Q319** One answer per stack; a stack whose files pass 6 MB fails ("too big to import through the Agent") instead
  of a new Agent → Control Plane blob transfer.
- **Q320** The CLI always sends the Host Key and the people's keys covering the Host, and `--host-only`; the Agent
  refuses to encrypt for its own Host Key alone (Q201's message) only when a stack has secrets, so a secret-free import
  needs no key list. `--recipient` still replaces the set, unchecked (Q294).
- **Q321** No local path: `import` always goes through the named Host's Agent (Q47), with the Agent's engine settings
  (rootless socket, Podman). Importing the machine you are on means enrolling its Agent first.
- **Q322** An Agent too old to import is told at once, from its Hello's version ("upgrade it to import"), instead of
  the request waiting for its timeout (Q62); a development build without a version is let through.
- **Q323** Issue #9: an `env_file`, Compose secret or config file under `../` is handled like one in the stack's
  folder: copied into the Application with its leading `../` dropped, encrypted when it holds secrets, the compose file
  pointing at the copy; a copy colliding with a file of the stack fails the stack, naming both.
- **Q324** A bind mount under `../` follows the same rules as one inside: a file (or read-only folder) up to 1 MB is
  config and copied, a writable or bigger folder is data with its absolute path on the host.
- **Q325** Data inside another tool's storage (`/var/lib/docker/volumes/`, `/var/lib/containers/storage/`, any
  volume's mountpoint the engine reports) fails the stack, naming the path and the volume: after the takeover it would
  vanish with that tool's cache (doco-cd's, issue #9).
- **Q326** Done when: the importer's tests pass through a fake Agent; an integration test imports through a real
  Agent; an e2e `import.sh` enrolls an Agent, imports hand-made stacks (one with `../` references, one with data in a
  volume) and takes them over; on Linux also through rootless Docker and Podman Hosts.

## Decrypted files as bind mounts (M6-10, 2026-11-23)

- **Q327** A relative bind source not in Git is accepted when a file from Git in its folder decrypts to it: one whose
  name contains `.sops.` and, with the first `.sops.` made `.`, is the source's name (`app.sops.yaml` → `app.yaml`).
  The rule lives once, in `secrets.DecryptedName`, used by the check and by the places that had copied it (decrypting,
  the work dir's pruning, the offline restore, the controller's key names). Anything else not in Git stays refused with
  the same message.
- **Q328** A plain file and the `.sops.` file that decrypts to it, both in Git (`app.yaml` beside `app.sops.yaml`), fail
  the render, naming both: decrypting would silently replace the file from Git (Q62).
- **Q329** No `:ro` required on a decrypted file's mount: as with a plain file from Git, a container's writes there last
  until the next Sync, which the README's data table says.
- **Q330** Tests: unit cases in the render's tests (decrypted source accepted, a `.sops.` file in another folder not
  counting, the Q328 conflict refused, the refusal message unchanged); `systemd.sh`'s app bind-mounts a plain file from
  Git and a decrypted file beside its Compose secret and reads all three, before and after the simulated reboot;
  `mounts.sh` keeps checking the refusal.
- **Q331** The README's Secrets example mounts a decrypted file beside `env_file` and `secrets:`, and its data table
  gains a row for it.

## Test pyramid with real Agents (M7-01, 2026-11-23)

- **Q332** The Control Plane driving the Agent on every engine: the Agent engines job runs every integration test of
  `agent`, `importer` and `engine` on each engine (rootful Docker, rootless Docker and rootless Podman in containers,
  the runner's own rootless Podman), not only `TestAgent*`; and one new integration test runs a real controller, hub,
  Source Server and Agent through an Application's life (create, Sync, drift and Self-Heal, Diff, Rollback, delete with
  volumes) on each. The e2e scripts stay on the runner's Docker: per engine they'd add ~20 minutes for what that test
  covers in-process.
- **Q333** `migrations.sh` on macOS: OrbStack's file sharing now and then serves a container started right after a
  rewrite the file's old content (the Agent's in-place write is correct; a fresh container sees it; never on Linux,
  where the engine reads the disk). Linux is where OmegaCD runs (Q61: macOS for development only), so there is no
  product change: on macOS the script skips the checks after a rewrite, naming the quirk, and Linux (CI, the gate) runs
  them all. The README's development notes say so.
- **Q334** Done when: the Agent engines job runs Q332's tests on all four engines, amd64 and arm64, and CI is green;
  `make e2e` is green on macOS with Q333's skip; the Linux gate passes.
## Ownership of Compose projects (M7-09, 2026-11-24)

Found on 2026-11-24: the e2e walkthrough's throwaway OmegaCD deployed an Application `whoami` to the same Docker as a
demo OmegaCD with an Application of that name; its Agent took the demo's container as its own (any OmegaCD's label
counted) and its delete removed it. In production two OmegaCDs, or two Hosts of one, on one engine would do the same,
and a delete with volumes would remove the other's data.

- **Q335** The Agent labels everything it creates for a project, containers and the volumes and networks Compose makes
  for it, with `dev.omegacd.owner=<Control Plane ID>/<Host name>` (Q341: the Control Plane ID alone): the Control Plane ID is random, made at its first
  start and kept in its database. It differs between two OmegaCDs and between two Hosts of one, and stays when a Host
  enrolls again with a new key (Q289).
- **Q336** A project whose containers carry another owner is never touched: a Sync, a Self-Heal or a delete of it is
  refused, naming the owner ("a Compose project whoami on this Host belongs to another OmegaCD (owner …): rename the
  Application, or set adopt: true to take it over"); `app delete --forget` still works, touching no container. A delete
  with volumes and Prune volumes remove only volumes with this owner.
- **Q337** Containers from before the owner label (OmegaCD's labels, no owner) are the Agent's own if its database has
  the Application, and the next Sync labels them; otherwise they are refused like a stack started by hand, and
  `adopt: true` takes them over. Docker can't relabel a container, so they get the label when Compose next recreates
  them; until then the database record is what makes them the Agent's own; adopt of another owner's project forces
  that recreate. Until now any OmegaCD's containers were taken silently.
- **Q338** A ticket of its own, M7-09, before M7-02: integration tests with two Agents on one engine and the same
  Application name (the second refused; the first's containers and volume untouched by the second's Sync, Self-Heal and
  delete with volumes; adopt taking over on purpose) and an e2e test with two OmegaCDs on one Docker.
- **Q339** The local e2e suite and a demo on the same Docker: the demo is stopped while the suite runs (the walkthrough
  uses the README's names); after M7-09 a collision is refused instead of destroying anything.
- **Q340** An Agent belongs to the first Control Plane that approves it, for good: it records that Control Plane's ID,
  which every Control Plane sends in its challenge, and refuses another one before enrolling (its invite stays unused),
  logging and telling it "this Agent belongs to OmegaCD <id>", which that Control Plane shows on the Host; it keeps its
  Applications running and self-healing meanwhile. A Control Plane restored from its backup keeps its ID; one rebuilt
  from nothing has a new one, which its Agents refuse: each is reinstalled with an empty state dir, a new Host Key and
  Q289's recovery (forget, add the Host again, rekey). This narrows Q117's recovery from nothing.
- **Q341** (corrects Q335) The owner is the Control Plane's ID alone, `dev.omegacd.owner=<Control Plane ID>`: Hosts are
  keyed by their name, which `host rename` changes, and Application names are unique in one OmegaCD, so two of its
  Hosts sharing an engine never collide, and an Application moved between them stays its own. The ID is 128 random
  bits made at the Control Plane's first start, unique, and never changed: no setting, rename or API sets it.
