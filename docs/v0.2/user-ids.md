# User ids, files and engines (draft for the v0.2 README)

How the user ids of the Agent, the container engine and the containers decide who can read and write which files.
Collected while building M2 (Q58, Q59); M7-05 folds it into the README.

## Two kinds of engine

**Rootful Docker**: `dockerd` runs as root; the Agent is a normal user in the `docker` group. A uid inside a
container *is* the same uid on the Host: container root is real root, container uid 1000 is Host uid 1000.

**Rootless Docker / Podman**: the engine runs as a normal user, and the Agent runs as that same user (a systemd user
unit). Ids are shifted: container root *is* that user; any other container uid is one of the user's spare ids
(`/etc/subuid`, e.g. container 999 → Host 100998), which belongs to nobody real.

| | Rootful | Rootless |
| --- | --- | --- |
| Container root is | real root on the Host | the Agent's user |
| Container uid 1000 is | Host uid 1000 | a spare id (≈ 100999) |
| The Agent's power | the `docker` group is root-equivalent (`docker run -v /:/host`) | only its own user |
| Images | all of them, privileged ports too | most; ports below 1024 need a sysctl |

## What OmegaCD writes, and who reads it

| Files | Owner, mode | Read by |
| --- | --- | --- |
| Work dir: files from Git (`<work dir>/<app>/`) | the Agent's user, `0644` / dirs `0755` | any container uid (world-readable); containers should mount them `:ro` |
| Decrypted secrets (`.env`, `tls/key.pem`, …) | the Agent's user, `0600` | Compose itself (the Agent) for `.env`; for a bind-mounted secret: containers running as root (rootful) or as root = the Agent's user (rootless) |
| Same, with `OMEGACD_AGENT_SECRET_GROUP=<gid>` | `0640`, that group | also non-root containers that join the group: `group_add: ["<gid>"]` rootful, `group_add: ["0"]` rootless (Q58) |
| Cache, database, keys (state dir) | the Agent's user, owner-only | the Agent only |

Data never goes in the work dir: every Sync makes it match Git. Data lives in named volumes or absolute paths.

**macOS** (Docker Desktop, OrbStack) shares host folders ignoring owner and mode: any container reads everything.
Hosts are Linux, where the modes above hold.

## Your data folders

An absolute path is created by the engine if missing: as **root** with rootful Docker, then a non-root image can't
write there. Create it once, owned by the uid the image runs as:

```sh
docker run --rm --entrypoint id postgres:17            # uid=999(postgres) …  (or the image's docs)
sudo install -d -o 999 -g 999 -m 700 /srv/miha/pgdata  # rootful
podman unshare chown 999:999 /srv/miha/pgdata          # rootless Podman
rootlesskit chown 999:999 /srv/miha/pgdata             # rootless Docker (as the rootless user)
```

`ls -l` then shows bare numbers (999, 101, or shifted ones like 100998 when rootless): normal, Linux only compares
numbers. Name them if it helps (`useradd --system --no-create-home --uid 999 pg-container`).

A **named volume** needs none of this: the engine gives it the image's ownership, rootful or rootless alike.

## The "user 1000" convention

Images built with a uid-1000 user, run on a Host where you are 1000:

| Setup | Works? |
| --- | --- |
| Rootful, Agent runs as you (1000) | yes: work dir, secrets and data are all yours |
| Rootful, Agent as a service user (e.g. 998) | Git files yes; bind-mounted secrets need `OMEGACD_AGENT_SECRET_GROUP` + `group_add` (or run the Agent as you) |
| Rootless | no: container 1000 isn't you. Let the image run as root (that root *is* you), or Podman `userns_mode: keep-id` |

OmegaCD refuses `build:`: push such images to a registry (Settings → Registry credentials for private ones).

## After a reboot (Q59)

With the work dir in RAM (the default) it is empty after a reboot and the Agent restores it from its cache. With
rootful Docker, containers with a restart policy start before the Agent and Docker creates their missing bind-mount
sources as root-owned empty directories: the system unit restores the files before Docker starts
(`omegacd agent restore --files`), a containerised Agent clears those placeholders through Docker. A work dir on disk
keeps its files, and the decrypted secrets in it, across reboots (the Agent warns).

## Choosing

- **Rootless** (Docker or Podman): safest; the Agent is one ordinary user and owns everything it writes.
- **Rootful + a docker-group Agent**: most compatible; the `docker` group is the trust you hand it.
