# OmegaCD v0.2: pull-based Agents

v0.2 is a breaking redesign: every Host runs an **Agent** that dials out to the **Control Plane** and runs Compose
itself, and a separate **Source Server** owns Git and rendering. v0.1 (push over SSH/TLS) continues on the
`release-0.1` branch, frozen and deprecated: there is no v0.1.0, and v0.2.0 is the first stable release (Q97).

| Read | For |
| --- | --- |
| [architecture.md](architecture.md) | the decided design: components, trust boundaries, flows, protocol, UI, install |
| [decisions.md](decisions.md) | every design decision (Q1–Q111) with its outcome: the "why" |
| [milestones/](milestones/) | the work, as tickets with dependencies and "done when" |
| [../adr/](../adr/) | the hard-to-reverse decisions (0003–0006) |
| [../../GLOSSARY.md](../../GLOSSARY.md) | the vocabulary: Host, Agent, Control Plane, Source Server, Enrollment, … |
| [user-ids.md](user-ids.md) | draft: user ids, file ownership and engines (rootful vs rootless), for the README |
| [original-spec.md](original-spec.md) | the input to the design review (superseded where it differs) |

## Milestones

Work them in order; a ticket may start once the tickets it depends on are done. **Before each milestone: a design
check** of its tickets against the decisions, the architecture and the roadmap (CLAUDE.md); between M2 and M3 the
roadmap's later features were decided first ([Q63–Q72](decisions.md#later-features-design-check-before-m3-2026-11-08), 2026-11-08); before M4,
[Q79–Q95](decisions.md#control-plane-in-pull-mode-design-check-before-m4-2026-11-09) (2026-11-09). M3-06 is done after
M4-01 (Q79); M4's order is M4-01, M3-06, M4-08, M4-09 (coverage, Q96), M4-02, M4-11 (Q98), then the rest, M4-10 (all code to 100%) after M4-05.

| # | Milestone | Tickets |
| --- | --- | --- |
| M1 | [Foundations](milestones/M1-foundations.md): release branch, protobuf schema, Source Server behind an interface, blob store, gRPC+mTLS, credentials | 6 (1 done) |
| M2 | [Agent core](milestones/M2-agent-core.md): `omegacd agent`, local DB, cache, work dir, decryption in memory, Compose, offline autonomy, offline rollback, per-Application work dir, logging everywhere, init services | 11 |
| M3 | [Connection](milestones/M3-connection.md): WebSocket, enrollment, signed challenges, version handshake, IP allowlist, resync | 6 |
| M4 | [Control Plane in pull mode](milestones/M4-control-plane-pull.md): Sync and status through Agents, Source Server for Git, the Control Plane's machine through a normal Agent, delete push, DR; deletes, registry credentials and live progress through Agents; coverage: measured combined, then 100% of all code; a new Revision's Diff on the Agent | 11 |
| M5 | [UI](milestones/M5-ui.md): Add Host, Host cards, badges, offline apps, Source Server settings, masked files, work dir field, "can't decrypt" | 8 |
| M6 | [Install and import](milestones/M6-install-import.md): install.sh modes, compose files, systemd, Kubernetes, import via Agent, proxy docs, RHEL family (deferred), secrets helper, .deb packages | 9 |
| M7 | [Hardening](milestones/M7-hardening.md): test pyramid incl. rootless Docker and Podman, e2e, chaos tests, real-world run, docs, RHEL-family matrix (deferred), Forgejo primary | 7 |
| M8 | [UI polish, then release](milestones/M8-ui-polish.md): UX, accessibility, performance, visuals, release notes, v0.2.0 | 6 |

A ticket is done when its "Done when" holds, proven by tests at every level: unit tests, an end-to-end test in
`test/e2e` through the real binary (or, for a piece with nothing to drive yet, in the first ticket that exposes it),
and the whole suite green: unit, integration and `make e2e` (Q50 adds the rest of the matrix in M7).

A ticket's **Status** line is the source of truth while it's worked on: set it to `in progress (branch)` when
starting and `done (date, PR)` when merged, in the same PR.

## GitHub issues

[create-issues.sh](create-issues.sh) turns the tickets into GitHub milestones and issues (needs `gh auth login`):

```sh
docs/v0.2/create-issues.sh            # dry run: prints what it would create
docs/v0.2/create-issues.sh --apply    # creates milestones and issues; skips ones that exist or are done
```
