# M5 · UI

The UI for Agents and the Source Server. See [architecture](../architecture.md#ui-q34-q37-q38-q39-q40-q43-q53).
Catppuccin, light/dark, phones, the existing animations; keep the house components.

### M5-01 · Add Host flow
**Status:** todo
**Depends on:** M3-02

Add Host → **name** (required; Git references Hosts by name) → install commands with the invite: compose snippet
(`deploy/agent.yaml`), `install.sh` one-liner, systemd (rootless, docker group) → live wait → key fingerprint →
**Approve** (Q37, Q43).

**Done when:** an admin can enroll a Host end to end from the UI, copying one command.

### M5-02 · Host cards
**Status:** todo
**Depends on:** M5-01, M3-04

Name and hostname, online or last seen, Agent version (outdated flag), engine badge (Docker/Podman + version),
rootless, OS/arch, number of apps, key fingerprint, **Rotate key**, **Revoke**, **Locked** with the reason and an
unlock action, IP allowlist editor; rename only while no Application uses the Host (Q37, Q53).

**Done when:** every field is live over SSE, and each action works with a confirmation where it's destructive.

### M5-03 · Badges everywhere
**Status:** todo
**Depends on:** M5-02, M4-02

Engine, Host offline, rolled back on the Host, queued Sync, Agent outdated: on app tiles, the app header, the tree's
Host node, lists and Host cards, with tooltips (Q34, Q39).

**Done when:** a visual check of every screen shows them, and a UI test covers their logic.

### M5-04 · Offline apps and queued Syncs
**Status:** todo
**Depends on:** M4-02

Last known Live State, Health, Diff and logs with "Host offline since …"; queued Syncs shown as waiting for the Host,
cancellable (Q38).

**Done when:** an offline Host's apps read clearly in the dashboard, tree and app page.

### M5-05 · Source Server settings and credentials
**Status:** todo
**Depends on:** M1-06

Settings → Source Server: in-process or standalone, reachable, version, cache size. Repository credentials pass
through to it; the access check in the app form asks it (Q40).

**Done when:** credentials can be added, rotated and deleted without the Control Plane ever storing a secret.

### M5-06 · Decrypted files from Agent-reported key names
**Status:** todo
**Depends on:** M2-05, M4-02

The file viewer shows `KEY=••••` from the key names the Agent reports; content of secret files is never shown.

**Done when:** no code path can send a decrypted value from Agent to Control Plane (test).

### M5-07 · Work dir in the Application form
**Status:** todo
**Depends on:** M2-09, M4-01

An optional **Work dir** field in the Application form and detail (Q57): empty shows the Host's work dir as the
placeholder; a path the Host's Agent doesn't allow shows the Agent's message and the setting to change.

**Done when:** UI tests cover empty, allowed and refused, and an e2e creates an Application with its own work dir.
