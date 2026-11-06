# OmegaCD

GitOps continuous delivery for Docker Compose hosts, in Go with an embedded SvelteKit UI. Public repo:
https://github.com/obleey/omegacd.

## Where things stand

- **`main` is v0.2 in development**: a breaking redesign to pull-based Agents and a separate Source Server. Read
  [docs/v0.2/README.md](docs/v0.2/README.md) first: architecture, every design decision (Q1–Q56), and the work as
  tickets in [docs/v0.2/milestones/](docs/v0.2/milestones/). Work milestones in order (M1 → M8).
- **`release-0.1`** is v0.1 (push over SSH/TLS, last tag `v0.1.0-rc.10`): fixes only, no features.
- Vocabulary: [GLOSSARY.md](GLOSSARY.md) (Host, Agent, Control Plane, Source Server, Application, Sync, …). Use its
  terms in code, UI and docs; challenge new words against it. Decisions that are hard to reverse: [docs/adr/](docs/adr/).

## Working on a ticket

1. Pick the next ticket whose dependencies are done; branch `feat/<ticket-id>-<slug>` (e.g. `feat/m1-02-proto`).
2. Set its **Status** line to `in progress (branch)`; when merged, `done (date, PR #)`, in the same PR.
3. Its "Done when" is the acceptance test: meet it, with tests.
4. Open a PR to `main`; CI must be green. The user reviews and merges unless they say otherwise.
5. If a decision in docs/v0.2 turns out wrong, stop and ask; then update decisions.md and architecture.md first.

## Rules

- **Commits:** author `Miha Oblišar <miha.oblishar@gmail.com>`; never the work address.
- **Never commit keys, tokens or passwords** (age keys, `.env`, credentials). gitleaks runs in CI on the full history.
- **Tools come from nix**, never Homebrew: add packages to `~/nix-config/common/packages.nix`, then the user runs
  `sudo darwin-rebuild switch --flake ~/nix-config#<host>`.
- **Stopping a local server by port:** kill only the listener, `kill $(lsof -nP -t -iTCP:8080 -sTCP:LISTEN)` (or
  `make stop`). Never `kill $(lsof -ti tcp:8080)`: it also kills OrbStack's VM manager and browser helpers, and all
  containers go down.
- **Kubernetes:** the user's kubeconfig holds production GKE contexts. Always pass `--context orbstack` (or the
  context the user names); never rely on the current context.
- **OrbStack quirk:** recreating a file at the same path within a second can mount a stale file into a container
  (macOS file sharing cache; Linux is unaffected). Tests that delete and recreate an app pause first.
- Keep code like the surrounding code: comment only the why; small diffs; README, TESTING.md and ROADMAP.md updated
  with user-visible changes.
- Before claiming something works, run it: unit tests at least; Docker integration and `make e2e` for engine,
  Agent or controller changes.

## Commands

```sh
make build              # web UI, then bin/omegacd
make test               # unit tests (go test ./...); cd web && npm run check && npm test for the UI
make test-integration   # against the local Docker engine (docker:dind for remote Hosts; ~6 min)
make e2e                # end-to-end: throwaway OmegaCDs driven through the CLI (needs age-keygen, sops)
make run                # run locally with data in ./data (OMEGACD_WORKDIR=… to move the work dir)
make stop               # stop it (only the listener)
docs/v0.2/create-issues.sh [--apply]   # tickets → GitHub issues (needs gh auth login)
```

Release: push a `v*` tag; CI builds binaries, the GitHub Release and `ghcr.io/obleey/omegacd` (multi-arch).
