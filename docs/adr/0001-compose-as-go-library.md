# Drive Compose as a Go library, not the CLI

omegacd embeds Docker Compose as a Go library (`github.com/docker/compose/v5`, pinned) rather than shelling out to `docker compose`. We get structured progress events for the UI, no CLI output parsing, no separate binary to keep in step with the image, and the same Docker host resolution (local socket, `ssh://`, TLS) for remote Hosts later.

## Consequences

- Heavy dependency tree (`docker/cli`, buildx-related modules) and no promised API stability: Compose upgrades are deliberate and gated by integration tests.
- Compose's per-service config hash lives in an `internal` package, so we vendor that function to decide Sync Status. An integration test asserts our hash equals the `com.docker.compose.config-hash` label Compose writes; if an upgrade changes the algorithm, CI fails instead of every Application turning OutOfSync. Fallback if this becomes a burden: ask Compose via a dry-run `Up`.

## Considered Options

- **Shell out to the `docker compose` CLI**: simpler dependencies, but brittle output parsing and a second artifact to version.
