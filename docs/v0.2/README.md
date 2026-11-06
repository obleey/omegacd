# OmegaCD v0.2: pull-based Agents

v0.2 is a breaking redesign: every Host runs an **Agent** that dials out to the **Control Plane** and runs Compose
itself, and a separate **Source Server** owns Git and rendering. v0.1 (push over SSH/TLS) continues on the
`release-0.1` branch for fixes only; both are released together when v0.2 is done.

| Read | For |
| --- | --- |
| [architecture.md](architecture.md) | the decided design: components, trust boundaries, flows, protocol, UI, install |
| [decisions.md](decisions.md) | every design decision (Q1–Q56) with its outcome: the "why" |
| [milestones/](milestones/) | the work, as tickets with dependencies and "done when" |
| [../adr/](../adr/) | the hard-to-reverse decisions (0003–0006) |
| [../../GLOSSARY.md](../../GLOSSARY.md) | the vocabulary: Host, Agent, Control Plane, Source Server, Enrollment, … |
| [original-spec.md](original-spec.md) | the input to the design review (superseded where it differs) |

## Milestones

Work them in order; a ticket may start once the tickets it depends on are done.

| # | Milestone | Tickets |
| --- | --- | --- |
| M1 | [Foundations](milestones/M1-foundations.md): release branch, protobuf schema, Source Server behind an interface, blob store, gRPC+mTLS, credentials | 6 (1 done) |
| M2 | [Agent core](milestones/M2-agent-core.md): `omegacd agent`, local DB, cache, work dir, decryption in memory, Compose, offline autonomy, offline rollback | 8 |
| M3 | [Connection](milestones/M3-connection.md): WebSocket, enrollment, signed challenges, version handshake, IP allowlist, resync | 6 |
| M4 | [Control Plane in pull mode](milestones/M4-control-plane-pull.md): Sync and status through Agents, Source Server for Git, built-in Agent, delete push, DR | 7 |
| M5 | [UI](milestones/M5-ui.md): Add Host, Host cards, badges, offline apps, Source Server settings, masked files | 6 |
| M6 | [Install and import](milestones/M6-install-import.md): install.sh modes, compose files, systemd, Kubernetes, import via Agent, proxy docs | 6 |
| M7 | [Hardening](milestones/M7-hardening.md): test pyramid incl. rootless Docker and Podman, e2e, chaos tests, real-world run, docs | 5 |
| M8 | [UI polish, then release](milestones/M8-ui-polish.md): UX, accessibility, performance, visuals, release notes, v0.1.0 + v0.2.0 | 6 |

A ticket's **Status** line is the source of truth while it's worked on: set it to `in progress (branch)` when
starting and `done (date, PR)` when merged, in the same PR.

## GitHub issues

[create-issues.sh](create-issues.sh) turns the tickets into GitHub milestones and issues (needs `gh auth login`):

```sh
docs/v0.2/create-issues.sh            # dry run: prints what it would create
docs/v0.2/create-issues.sh --apply    # creates milestones and issues; skips ones that exist or are done
```
