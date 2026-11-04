# dockercd

GitOps continuous delivery for Docker — ArgoCD, but for Docker Compose.

dockercd watches Git repositories and keeps Compose projects running on your Docker hosts in line with what's committed. Point an Application at a path in a repo, and dockercd shows whether it's in sync, how healthy it is, and syncs it — manually or automatically.

> Status: early development, MVP in progress.

## Features (MVP)

- **Applications** — a Compose project (one or more compose files + profiles) at a path in a Git repo, tracking a branch, tag or commit
- **Sync** — manual or Auto-Sync, with optional Self-Heal and Prune (volumes are never pruned)
- **Sync & Health status** — Synced / OutOfSync / Unknown; Healthy / Progressing / Degraded / Missing
- **History & Rollback** — every sync recorded; roll back to any earlier revision
- **Secrets** — SOPS + age: `*.sops.*` files are decrypted at sync time and never written to disk in plaintext
- **Change detection** — Git polling plus GitHub, GitLab and Gitea/Forgejo webhooks
- **Web UI** — resource tree, diffs, live container logs, Catppuccin theme
- **API** — REST + Server-Sent Events, API tokens for CI
- **Metrics** — Prometheus `/metrics`

Images are built in CI; dockercd only pulls and runs them (`build:` is not supported).

## Quick start

```yaml
# compose.yaml
services:
  dockercd:
    image: ghcr.io/obleey/dockercd:latest
    ports:
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - dockercd-data:/data
      - ./age.key:/etc/dockercd/age.key:ro
      # Working dir must have the same path on the host and in the container:
      # bind mounts in your compose files are resolved by the Docker host.
      - /run/dockercd:/run/dockercd
    restart: unless-stopped

volumes:
  dockercd-data:
```

```sh
age-keygen -o age.key   # also the key your *.sops.* files are encrypted to
docker compose up -d
docker compose logs dockercd | grep "initial admin password"
```

Open http://localhost:8080, log in as `admin`, and change the password.

### Git over SSH

SSH host keys are always verified. Mount a `known_hosts` file at `/etc/dockercd/known_hosts` (or set `DOCKERCD_SSH_KNOWN_HOSTS`):

```sh
ssh-keyscan github.com gitlab.com >> known_hosts   # verify the fingerprints against your provider's docs
```

## Development

Requires Go, Node.js and a running Docker engine.

```sh
make build   # builds the web UI, then the Go binary into bin/
make test    # unit + integration tests (integration tests need Docker)
```

## License

Apache-2.0
