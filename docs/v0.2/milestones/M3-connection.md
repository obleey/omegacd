# M3 · Connection

The Agent ↔ Control Plane link: one WebSocket, enrollment, authentication, versions.
See [architecture](../architecture.md#the-connection-q16-q20-q22-q27-q28-q41-q42-q51).

### M3-01 · The WebSocket
**Status:** todo
**Depends on:** M1-02

`/api/agents/v1/connect` on the Control Plane and the Agent's client (Q20, Q51): binary frames carrying the protobuf
envelope; ping every 30 s; reconnect with exponential backoff and jitter; large payloads in chunks so status and
logs are never stuck behind them; transfers resume by hash after a reconnect. Works through Caddy, Traefik and nginx
as reverse proxies.

**Done when:** tests cover reconnect, a dropped connection mid-transfer resuming, and a proxy idle timeout of 60 s
never cutting a quiet connection; a test runs through a real reverse proxy container.

### M3-02 · Enrollment with invites and approval
**Status:** todo
**Depends on:** M3-01

An admin adds a Host by **name** → single-use, short-lived invite stored as a hash (Q22, Q43). The Agent starts with
it, generates an ed25519 signing key (and has its age Host Key, M2-05), registers both public keys; the invite is
used up. The Host waits for **approval** with the key fingerprint shown in the UI and printed by the Agent (Q28).

**Done when:** a used or expired invite is refused, an unapproved Agent can't receive anything, and the fingerprint
matches on both sides.

### M3-03 · Signed challenges, locking, rotation, revocation
**Status:** todo
**Depends on:** M3-02

Every connection authenticates by signing a fresh challenge (Q22); no bearer tokens. A second live connection with
the same key **locks** the Host until an admin acts (Q28). Key rotation (the Agent generates a new key, signs the
change with the old one) and revocation (delete the Host). Keys owner-only on disk; optional sealing with
`systemd-creds`/TPM.

**Done when:** tests cover a replayed signature (refused), a duplicate connection (locked), rotation without a gap,
and revocation closing the live connection.

### M3-04 · Version handshake
**Status:** todo
**Depends on:** M3-01

Hello carries Agent and protocol versions (Q41): Agents one minor version behind are served; older are refused with a
message saying what to upgrade, and keep running their apps. The Control Plane records each Agent's version.

**Done when:** tested for same, minus one and minus two minor versions.

### M3-05 · IP allowlist per Host
**Status:** todo
**Depends on:** M3-03

Optional CIDR list per Host (Q28); the client address comes through `OMEGACD_TRUSTED_PROXIES` like today's API.

**Done when:** an Agent outside the list is refused even with a valid key, behind a proxy as well.

### M3-06 · Resync and fallback poll
**Status:** todo
**Depends on:** M3-01, M2-02

On connect and every few minutes the Agent sends per-app checksums; the Control Plane answers with what differs (Q16):
the safety net for missed signals, and how the outbox gets drained after offline time.

**Done when:** a signal dropped on purpose is healed by the next resync, and outbox entries arrive exactly once.
