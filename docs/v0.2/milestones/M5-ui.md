# M5 · UI

The UI for Agents and the Source Server. See [architecture](../architecture.md#ui-q34-q37-q38-q39-q40-q43-q53) and the
design check before M5 (Q151–Q158). Catppuccin, light/dark, phones, the existing animations; keep the house components.
UI logic in pure `.ts` modules tested with `node --test`; each PR shows screenshots of what it touched, light, dark and
at phone width (Q158).

### M5-01 · Add Host flow
**Status:** done (2026-11-17, PR #142)
**Depends on:** M3-02

Add Host → **name** (required; Git references Hosts by name) → install commands with the invite → live wait → key
fingerprint → **Approve** (Q37, Q43). The commands are those that work before M6 (Q151): the `omegacd agent` command
line `host add` prints, and a `docker run` of the published image with the same settings; M6-01 (`install.sh`), M6-02
(`deploy/agent.yaml`) and M6-03 (systemd) each add their tab. On the Hosts page (Q152), waiting live through the
`host` event (Q153).

**Done when:** an admin can enroll a Host end to end from the UI, copying one command.

### M5-02 · Host cards
**Status:** done (2026-11-17, PR #143)
**Depends on:** M5-01, M3-04

Name and hostname, online or last seen, Agent version (outdated flag), engine badge (Docker/Podman + version),
rootless, OS/arch, number of apps, key fingerprint, **Rotate key**, **Revoke**, **Locked** with the reason and an
unlock action, IP allowlist editor; rename only while no Application uses the Host (Q37, Q53).
The Host's **public Host Key** (`age1…`, the Agent's, registered at enrollment) with **one-click copy**, and its
`.sops.yaml` rule ready to copy, plus **Copy .sops.yaml for all Hosts**, as v0.1's Keys dialog has: so developers
encrypt secrets for a Host without asking anyone (the CLI has it in `omegacd host list`; M6-08 writes the file).
Cards replace M5-01's table (Q161); rename through the API and `omegacd host rename` (Q162); `outdated` decided by
the Control Plane (Q163); confirmations (Q164); the allowlist editor shows the Agent's last address (Q165); the e2e
(Q166).
A Date and time format setting, per browser: Browser default, European 24-hour, US 12-hour (Q168–Q170).

**Done when:** every field is live over SSE, each action works with a confirmation where it's destructive, and a
secret encrypted with the copied key (or the copied `.sops.yaml` rule) deploys to that Host.

### M5-03 · Badges everywhere
**Status:** done (2026-11-18, PR #145)
**Depends on:** M5-02, M4-02

Engine, Host offline, rolled back on the Host, queued Sync, Agent outdated: on app tiles, the app header, lists and
Host cards, with tooltips (Q34, Q39).
Worked out in the UI from the Hosts store (Q173); a rollback on the Host recorded as `host-rollback` (Q174); no Host
node in the tree (Q175); where each shows (Q176); offline views stay M5-04 (Q177); a Host card's
Applications link to them (Q178); containers' uptime counted live from the start time their Agent reports (Q179).

**Done when:** a visual check of every screen shows them, and a UI test covers their logic.

### M5-04 · Offline apps and queued Syncs
**Status:** todo
**Depends on:** M4-02

Last known Live State, Health and Diff with "Host offline since …"; the Logs tab says logs come from the Agent (no logs
are stored, Q155); queued Syncs shown as waiting for the Host, cancellable (Q38; `POST /api/apps/{name}/syncs/{id}/cancel`
finishes one Superseded, "cancelled by admin", Q154).
While the Host is offline, a container's uptime says "as of" the Agent's last report (Q179; the live count is M5-03's).

**Done when:** an offline Host's apps read clearly in the dashboard, tree and app page.

### M5-05 · Source Server settings and credentials
**Status:** todo
**Depends on:** M1-06

Settings → Source Server: in-process or standalone, reachable, version, cache size, from a new `Status` RPC
(Q156). Repository credentials pass through to it; the access check in the app form asks it (Q40).

**Done when:** credentials can be added, rotated and deleted without the Control Plane ever storing a secret.

### M5-06 · Decrypted files from Agent-reported key names
**Status:** todo
**Depends on:** M2-05, M4-02

The file viewer shows `KEY=••••` from the key names the Agent reports; content of secret files is never shown.
From M4-04: `test/e2e/secrets.sh` checks only that a decrypted file's content isn't shown; it checks `API_TOKEN=••••`
again with this ticket.

**Done when:** no code path can send a decrypted value from Agent to Control Plane (test).

### M5-07 · Work dir in the Application form
**Status:** todo
**Depends on:** M2-09, M4-01

An optional **Work dir** field in the Application form and detail (Q57): empty shows the Host's work dir as the
placeholder; a path the Host's Agent doesn't allow shows the Agent's message and the setting to change. The Agent's
Hello carries its work dir and allowed roots (Q157).

**Done when:** UI tests cover empty, allowed and refused, and an e2e creates an Application with its own work dir.

### M5-08 · "This Host can't decrypt it"
**Status:** todo
**Depends on:** M2-05, M4-02

When a secret source on the Agent can't produce a secret, the Sync fails and the Application shows why, per source:
for SOPS, the file was encrypted for other keys than this Host's (the error names the file, this Host's public key
and the fix: add the key to `.sops.yaml` and run `sops updatekeys`, or `omegacd secrets init`, M6-08); for the
sources that come later (KeePass, 1Password, Ansible Vault, OpenBao, …, Q29) their own reasons (no access, an entry
that doesn't exist), reported through the same field so the UI needs nothing new per source.

**Done when:** a file encrypted for another Host shows that reason with the fix in the UI and the CLI, the message
never contains a secret, and a test with the dummy second source (M2-05) shows its reason the same way.

### M5-09 · Rollback of a Child Application
**Status:** todo
**Depends on:** M5-02

A Child Application is rolled back as any Application, as in Argo CD (Q171): an older Revision from its history, its
Auto-Sync off. While it is rolled back, a Sync of its App of Apps neither syncs it nor turns its Auto-Sync back on;
a manual Sync of the child ends it. Its page says it is rolled back and why its App of Apps skips it; the README's
"Rollback is owned by Git" goes. With Q167 (M6-05): a child with `adopt: true` never synced is held back the same way,
so both are tested together (Q172).

**Done when:** a child is rolled back from the UI and the CLI, a Sync of its App of Apps leaves it rolled back, a
manual Sync of the child ends it, and an e2e drives it through an Agent.

### M5-10 · pnpm for the web UI
**Status:** todo
**Depends on:** M5-03

The web UI moves from npm to pnpm (Q183): `pnpm-lock.yaml` replaces `package-lock.json` (the same versions, imported
with `pnpm import`), pnpm comes from nix (`~/nix-config/common/packages.nix`) and from a pinned `corepack` in CI and
the Dockerfile, and the Makefile, `.github/workflows/ci.yml`, `release.yml`, CLAUDE.md and TESTING.md say `pnpm`.

**Done when:** `make build`, CI, the image build and the release workflow use pnpm, no `package-lock.json` is left,
and the built UI is the same.
