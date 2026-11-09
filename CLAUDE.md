# OmegaCD

GitOps continuous delivery for Docker Compose hosts, in Go with an embedded SvelteKit UI. Public repo:
https://github.com/obleey/omegacd.

## Where things stand

- **`main` is v0.2 in development**: a breaking redesign to pull-based Agents and a separate Source Server. Read
  [docs/v0.2/README.md](docs/v0.2/README.md) first: architecture, every design decision (Q1–Q97), and the work as
  tickets in [docs/v0.2/milestones/](docs/v0.2/milestones/). Work milestones in order (M1 → M8).
- **v0.1 is deprecated** (Q97): `release-0.1` (push over SSH/TLS, last tag `v0.1.0-rc.10`) is frozen, no fixes; there
  is no v0.1.0 release. v0.2.0 is OmegaCD's first stable release. Write no tests or features for v0.1-only code: M4-05
  deletes it.
- Vocabulary: [GLOSSARY.md](GLOSSARY.md) (Host, Agent, Control Plane, Source Server, Application, Sync, …). Use its
  terms in code, UI and docs; challenge new words against it. Decisions that are hard to reverse: [docs/adr/](docs/adr/).

## Working on a ticket

1. Pick the next ticket whose dependencies are done; branch `feat/<ticket-id>-<slug>` (e.g. `feat/m1-02-proto`).
2. Set its **Status** line to `in progress (branch)`; when merged, `done (date, PR #)`, in the same PR.
3. Its "Done when" is the acceptance test: meet it, with tests at every level (see **Testing** below).
4. Open a PR to `main`; CI must be green. The user reviews and merges unless they say otherwise.
5. If a decision in docs/v0.2 turns out wrong, stop and ask; then update decisions.md and architecture.md first.

**Design checks** (grilling), not after every ticket but:
- **Before each milestone starts**: walk its tickets against decisions.md, architecture.md and ROADMAP.md (later items
  too: they can change what's built now) and settle what's open, recorded as decisions. Between M2 and M3 that was the
  roadmap's later features, decided as [Q63–Q72](docs/v0.2/decisions.md#later-features-design-check-before-m3-2026-11-08).
- **When a ticket shows a decision is wrong** (step 5): a focused grilling on that question before going on.
- **Before building something hard to reverse** (the protocol, a database schema, the install path): a full grilling.

## Rules

- **Commits:** author `Miha Oblišar <miha.oblishar@gmail.com>`; never the work address. No "Generated with Claude
  Code" or "Co-Authored-By: Claude" lines in commit messages or PR descriptions.
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
- **Testing, for every feature and fix:** unit tests for its logic, an end-to-end test in `test/e2e` that drives it
  through the real binary (a feature with nothing to drive yet, like an internal package, gets its e2e with the first
  ticket that exposes it, and that ticket's PR says so), and before **every push** (fixes to an open PR too), after
  the last change, the **whole** suite green on Linux (`make test-linux`; macOS hides permission bugs):
  `go test -race ./...`, `make test-integration`, `make test-engines` for Agent changes, and `make e2e` (start Forgejo first:
  `docker compose -f test/e2e/forgejo.yaml up -d`, so the webhook test runs instead of skipping). The PR says what ran.
  **Coverage (Q96): 100% of all reachable code, existing code included**, unit + integration + e2e combined
  (generated code excluded). A line no test can reach (an error that can't happen) is listed with why in
  `test/coverage-exceptions.txt`, reviewed like code; every other uncovered line fails CI (`make cover`, M4-09). Until
  M4-10 brings existing code there, every PR covers 100% of its reachable changed lines and lowers nothing.
  A change that touches no shipped code (docs, the plan, test-only fixes) skips the Linux gate: the changed tests run
  locally, CI checks the push.
  **Two loops:** a target that fails is stopped at its first failure (the others go on); fix it by rerunning only the
  failing part on that target (`test/linux.sh --only "CMD" TARGET`); then, before the push, the whole suite on all
  targets again.
  Within `make test-all` a test whose environment is missing (dind, sops, the docker CLI) fails instead of skipping
  (`skipUnlessRequired`): only true platform skips stay (macOS, the matrix's other-user engines).

## Commands

```sh
make build              # web UI, then bin/omegacd
make proto              # protobuf → internal/proto (commit the result; rules in proto/README.md)
make test               # unit tests (go test ./...); cd web && npm run check && npm test for the UI
make test-integration   # against the local Docker engine (docker:dind for remote Hosts; ~6 min)
make test-engines       # the Agent against rootful Docker, rootless Docker and rootless Podman (test/engines.sh)
make test-all           # the whole suite: vet, unit -race, integration, test-engines, e2e with Forgejo
make test-linux         # test-all on Ubuntu 24.04 and Debian 13, amd64 and arm64: Hetzner servers (test/linux.sh, ../omegacd-testinfra)
make e2e                # end-to-end: throwaway OmegaCDs driven through the CLI (needs age-keygen, sops)
make run                # run locally with data in ./data (OMEGACD_WORKDIR=… to move the work dir)
make stop               # stop it (only the listener)
docs/v0.2/create-issues.sh [--apply]   # tickets → GitHub issues (needs gh auth login)
```

Release: push a `v*` tag; CI builds binaries, the GitHub Release and `ghcr.io/obleey/omegacd` (multi-arch).
