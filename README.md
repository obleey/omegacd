<p align="center">
  <img src="assets/logo-wordmark.svg" alt="OmegaCD" width="360">
</p>

<p align="center">
  <b>GitOps continuous delivery for Docker Compose hosts.</b>
</p>

<p align="center">
  <a href="https://github.com/obleey/omegacd/actions/workflows/ci.yml"><img src="https://github.com/obleey/omegacd/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/obleey/omegacd" alt="License: Apache-2.0"></a>
  <a href="https://github.com/obleey/omegacd/releases/latest"><img src="https://img.shields.io/github/v/release/obleey/omegacd?sort=semver" alt="Latest release"></a>
  <a href="https://github.com/obleey/omegacd/pkgs/container/omegacd"><img src="https://img.shields.io/badge/ghcr.io-obleey%2Fomegacd-blue?logo=docker" alt="Container image on GHCR"></a>
</p>

OmegaCD watches Git repositories and keeps Docker Compose projects on your Docker hosts in line with what is committed. Point an **Application** at a path in a repo; OmegaCD shows whether it is in sync and healthy, shows the diff, and syncs it — manually, or automatically on every push.

![Applications overview](assets/screenshots/applications-dark.png)

> **Status: early.** OmegaCD works and is tested against real Docker engines, but it is young and the API may still change. Feedback, bug reports and ideas are very welcome — please [open an issue](https://github.com/obleey/omegacd/issues).

## Why

Plenty of apps run on plain Docker hosts — a VPS, a homelab box, a few servers for a small team or an agency's clients — with no Kubernetes in sight. Deploying usually means SSH-ing in and running `git pull && docker compose up -d`, and nobody can say for sure what is running, what changed, or how to get back to yesterday's version. OmegaCD makes Git the answer:

- **Git is the single source of truth.** What is committed is what runs; a deploy is a commit.
- **See the diff before you deploy.** Per-service changes and the Git diff between the deployed and the target Revision.
- **Drift detection and Self-Heal.** A container changed or removed by hand shows up as OutOfSync, and OmegaCD can put it back.
- **History and Rollback.** Every Sync is recorded; roll back to any earlier Revision.
- **Secrets encrypted in Git.** SOPS + age files live next to your compose files and are decrypted only at Sync time.
- **Many hosts from one place.** The local engine and remote engines over SSH or TLS, in one UI, API and CLI.

It stays small: one binary in one container, no database server, no agents on your hosts, and your compose files unchanged. OmegaCD runs Compose as a library, so whatever `docker compose up` accepts works (except `build:` — build images in CI; OmegaCD pulls and runs them).

**Inspired by** the GitOps tools of the Kubernetes world — [Argo CD](https://argo-cd.readthedocs.io/) and [Flux](https://fluxcd.io/) — and the [OpenGitOps principles](https://opengitops.dev/) (declarative, versioned and immutable, pulled automatically, continuously reconciled), brought to Docker Compose. OmegaCD is an independent project, not affiliated with Argo CD, Flux or the CNCF.

## Features

- **Applications** — a Compose project (one or more compose files, optional profiles) at a path in a Git repo, tracking a branch, tag or commit, deployed to one Host
- **Sync Status and Health Status** — Synced / OutOfSync / Unknown; Healthy / Progressing / Degraded / Missing / Unknown — live over Server-Sent Events, resynced after reconnects
- **Sync Policy** — manual Sync, or Auto-Sync on new commits, Self-Heal on drift, and Prune
- **Diff** — Live (the compose files on the Host) and Desired (the compose files in Git) side by side and as a Compose diff, plus the per-service changes and the Git diff between the deployed and the target Revision
- **History and Rollback** — every Sync Operation is recorded; roll back to any earlier Revision
- **App of Apps** — declare Applications in Git; one parent Application creates, updates and deletes its children
- **Remote Hosts** — Docker engines over `ssh://` (host keys verified) or `tcp://` with TLS
- **Secrets** — SOPS + age encrypted files in Git, decrypted only at Sync time
- **Change detection** — Git polling plus GitHub, GitLab and Gitea/Forgejo webhooks
- **Web UI** — resource tree, diffs, colored live container logs, syntax colors for compose and config files (YAML, .env, nginx, Apache, Caddyfile, TOML, Markdown… ~240 formats via Shiki), repository access check while creating an Application, links to the Git host, light/dark/system theme, works on phones
- **CLI and API** — REST API with API tokens; a CLI for scripts and CI (`omegacd app sync myapp --wait`)
- **Prometheus metrics** at `/metrics`

| Application detail: resource tree and Sync Policy | Compose diff of a pending change |
| --- | --- |
| ![Application detail](assets/screenshots/app-detail.png) | ![Diff](assets/screenshots/diff.png) |

| Light theme | On a phone |
| --- | --- |
| ![Applications, light theme](assets/screenshots/applications-light.png) | <img src="assets/screenshots/mobile.png" alt="Applications on a phone" width="300"> |

![Live container logs with ANSI colors and highlighted warnings and errors](assets/screenshots/logs.png)

## Install

OmegaCD needs a Docker engine and an [age](https://github.com/FiloSottile/age) key. The key seals stored credentials and decrypts your SOPS secrets, so **back it up** — without it, stored credentials and encrypted secrets cannot be read.

**Quick install** — on a Linux server with Docker, or into a Kubernetes cluster with `kubectl`:

```sh
curl -fsSL https://raw.githubusercontent.com/obleey/omegacd/main/deploy/install.sh | sh
```

[`install.sh`](deploy/install.sh) asks for the port and the work dir (Compose) or the kubectl context and namespace (Kubernetes, always confirmed), creates the Master Key, starts OmegaCD and prints the URL, the initial admin password and the public key for `.sops.yaml`. It needs no `age` or `sops` on the server (`omegacd keygen` makes the key), never replaces an existing key, and runs unattended with `OMEGACD_TARGET`, `OMEGACD_VERSION` and the settings listed at its top. The options below do the same by hand.

### Option 1: Docker Compose (recommended)

Download [`deploy/compose.yaml`](deploy/compose.yaml). It mounts the Docker socket, a data volume, the age key, `known_hosts` and the work dir (at the same path inside and outside, see below). Every setting has a default you can override with environment variables or a `.env` file next to it: `OMEGACD_IMAGE` (pin a release, e.g. `ghcr.io/obleey/omegacd:0.1.0`), `OMEGACD_PORT`, `OMEGACD_WORKDIR`, `OMEGACD_DATA` (a folder instead of the volume), `OMEGACD_AGE_KEY`, `OMEGACD_KNOWN_HOSTS`.

```sh
curl -fsSLO https://raw.githubusercontent.com/obleey/omegacd/main/deploy/compose.yaml
```

```sh
age-keygen -o age.key && chmod 600 age.key   # the Master Key: seals OmegaCD's stored secrets; Host Key of the local Host
touch known_hosts                             # add hosts with ssh-keyscan when you use Git or Hosts over SSH
docker compose up -d
docker compose logs omegacd | grep "initial admin password"
```

Open http://localhost:8080, log in as `admin` with the password from the log, and set a new password (required on first login). Put OmegaCD behind a TLS-terminating reverse proxy before exposing it.

**Why `/run/omegacd:/run/omegacd`?** On Sync, OmegaCD writes each Application's files (compose files, configs, decrypted secrets) to its work dir and asks the Docker engine to start the containers. Bind mounts like `./nginx.conf:/etc/nginx/nginx.conf` are resolved by the *engine*, on the host — so the work dir must exist at the same path on the host as inside the OmegaCD container.

**After a reboot** `/run` is empty again (it's RAM-backed, so decrypted secret *files* never touch disk — see [what reaches the disk](#what-reaches-the-disk)). Docker restarts your containers before OmegaCD is up, and their bind-mounted files are missing. On startup OmegaCD notices, restores every Application's files from its *deployed* Revision (nothing new is deployed), re-decrypts secrets and recreates the containers — it shows in History as a `restore` Sync. This covers the machine OmegaCD runs on (its work dir is `OMEGACD_WORKDIR`, shown read-only under **Settings → Hosts**); [remote Hosts](#remote-hosts) use a persistent work dir instead.

**A persistent work dir instead:** point `OMEGACD_WORKDIR` at a disk path such as `/opt/services` and mount it at the same path (`/opt/services:/opt/services`). The files then survive reboots and no `restore` Sync is needed, but decrypted secret files sit on that disk (mode 0600) and end up in its backups. Either way the work dir is OmegaCD's copy of Git, not a place for data: every Sync makes it match Git again (see [Where your data lives](#where-your-data-lives)). Changing the path is safe: on its first start with the new path OmegaCD finds no files there and restores every Application into it, recreating the containers once.

**Upgrade:** bump the image tag (or `docker compose pull` for `latest`) and run `docker compose up -d`. Data lives in the `omegacd-data` volume.

### Option 2: Binary + systemd

Download the `linux_amd64` or `linux_arm64` archive from [GitHub Releases](https://github.com/obleey/omegacd/releases) and verify it:

```sh
VERSION=v0.1.0 ARCH=amd64
curl -LO https://github.com/obleey/omegacd/releases/download/$VERSION/omegacd_${VERSION}_linux_${ARCH}.tar.gz
curl -LO https://github.com/obleey/omegacd/releases/download/$VERSION/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf omegacd_${VERSION}_linux_${ARCH}.tar.gz
sudo install -m 0755 omegacd_${VERSION}_linux_${ARCH}/omegacd /usr/local/bin/omegacd
```

Create a service user in the `docker` group and its key files:

```sh
sudo useradd --system --home-dir /var/lib/omegacd --shell /usr/sbin/nologin --groups docker omegacd
sudo install -d -m 0750 -o omegacd -g omegacd /etc/omegacd
sudo -u omegacd age-keygen -o /etc/omegacd/age.key
sudo -u omegacd install -m 0600 /dev/null /etc/omegacd/known_hosts
sudo chmod 600 /etc/omegacd/age.key
```

Install the unit from [`packaging/omegacd.service`](packaging/omegacd.service) and start it:

```sh
sudo curl -Lo /etc/systemd/system/omegacd.service https://raw.githubusercontent.com/obleey/omegacd/main/packaging/omegacd.service
sudo systemctl daemon-reload
sudo systemctl enable --now omegacd
journalctl -u omegacd | grep "initial admin password"
```

<details>
<summary><code>packaging/omegacd.service</code></summary>

```ini
[Unit]
Description=OmegaCD - GitOps continuous delivery for Docker Compose
Documentation=https://github.com/obleey/omegacd
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=simple
User=omegacd
Group=omegacd
SupplementaryGroups=docker
ExecStart=/usr/local/bin/omegacd server
Restart=on-failure
RestartSec=5

Environment=OMEGACD_ADDR=:8080
Environment=OMEGACD_DATA=/var/lib/omegacd
Environment=OMEGACD_WORKDIR=/run/omegacd
Environment=OMEGACD_AGE_KEY_FILE=/etc/omegacd/age.key
Environment=OMEGACD_SSH_KNOWN_HOSTS=/etc/omegacd/known_hosts
EnvironmentFile=-/etc/omegacd/omegacd.env

StateDirectory=omegacd
StateDirectoryMode=0750
RuntimeDirectory=omegacd
RuntimeDirectoryMode=0750
RuntimeDirectoryPreserve=yes

NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
```

</details>

Further settings (webhook secret, trusted proxies, …) go into `/etc/omegacd/omegacd.env` (mode 0600). Remote `ssh://` Hosts need the `ssh` client installed. **Upgrade:** replace `/usr/local/bin/omegacd` and `systemctl restart omegacd`.

### Option 3: CLI only

For laptops and CI, download the `darwin_*` or `linux_*` archive from [Releases](https://github.com/obleey/omegacd/releases) and put `omegacd` on your `PATH`. It is the same binary as the server — see [CLI](#cli).

### Option 4: Kubernetes

OmegaCD can run in a cluster as the control plane, managing Docker hosts as [remote Hosts](#remote-hosts) over SSH or TLS. Kubernetes nodes run containerd, not Docker, so the manifest sets `OMEGACD_LOCAL_HOST=false`: the built-in `local` Host is not shown, and Applications must name a `host:`. [`deploy/kubernetes.yaml`](deploy/kubernetes.yaml) has a PersistentVolumeClaim, a single-replica Deployment and a Service:

```sh
kubectl create namespace omegacd
kubectl -n omegacd create secret generic omegacd-keys --from-file=age.key --from-file=known_hosts
kubectl -n omegacd apply -f https://raw.githubusercontent.com/obleey/omegacd/main/deploy/kubernetes.yaml
kubectl -n omegacd logs deploy/omegacd | grep "initial admin password"
```

Keep `replicas: 1` (one controller per SQLite database) and the work dir on the volume (`OMEGACD_WORKDIR=/data/work`): with an empty work dir OmegaCD assumes a reboot and recreates every Application's containers. Add the commented Ingress for your host name, and set `OMEGACD_TRUSTED_PROXIES` to the ingress controller.

### Versions

The version is always the Git release tag: pushing tag `v0.1.0` builds and stamps the binaries (`omegacd version` → `omegacd v0.1.0`) and the image (`ghcr.io/obleey/omegacd:0.1.0`, `:0.1` and `:latest`). The server reports it at `GET /api/version`, and the UI shows it in the sidebar and on the login page.

## Your first Application

This repository has a ready example, [`examples/hello`](examples/hello): nginx serving a page that lives in Git.

```yaml
# examples/hello/compose.yaml
services:
  web:
    image: nginx:1.29-alpine
    ports:
      - "8081:80"
    volumes:
      - ./index.html:/usr/share/nginx/html/index.html:ro # a file from Git, restored on every Sync
    restart: unless-stopped
```

**In the UI:** **New Application** → name `hello`, repository `https://github.com/obleey/omegacd`, path `examples/hello`, turn on **Auto-Sync**, **Create**. OmegaCD deploys it within seconds; open http://localhost:8081 (or the server's address). The Application page shows its containers, the Diff, History and Logs.

**With the CLI** (`omegacd login` first, see [CLI](#cli)):

```sh
omegacd app create hello --repo https://github.com/obleey/omegacd --path examples/hello --auto-sync
omegacd app wait hello --sync --health
```

**Make it yours:** copy `examples/hello` into your own repository, point the Application at it, change `index.html` and push — OmegaCD sees the new commit (on the next poll, or within seconds [with a webhook](#webhooks)) and recreates the container with the new page. Add a `.sops.env` next to it for [secrets](#secrets).

### Your first App of Apps

Instead of creating each Application by hand, declare them in Git. [`examples/apps.yaml`](examples/apps.yaml) defines two: `hello` (Auto-Sync on) and [`whoami`](examples/whoami) (Auto-Sync off — it waits for a Sync):

```yaml
# examples/apps.yaml
name: hello
repoURL: https://github.com/obleey/omegacd.git
path: examples/hello
host: local
autoSync: true
selfHeal: true
---
name: whoami
repoURL: https://github.com/obleey/omegacd.git
path: examples/whoami
host: local
autoSync: false
```

**New Application** → name `examples`, kind **App of Apps**, repository `https://github.com/obleey/omegacd`, path `examples`, **Create**, then **Sync** it (or `omegacd app create examples --repo https://github.com/obleey/omegacd --path examples --kind apps && omegacd app sync examples --wait`). The Sync creates both Child Applications and deploys them — http://localhost:8081 and http://localhost:8082. From then on a new commit deploys `hello` by itself, while `whoami` shows *OutOfSync* until you Sync it (or the App of Apps). In your own repository, one folder of definitions like this per server describes everything that runs there; see [Defining Applications](#defining-applications) for every field.

## Concepts

| Term | Meaning |
| --- | --- |
| **Application** | A Compose project whose desired state lives at a path in a Git repository and is deployed to one Host. |
| **App of Apps** | An Application whose path holds Application definitions instead of compose files. Syncing it creates and updates its Child Applications, and with Prune deletes those no longer declared. It deploys nothing itself and has no Host: each Child Application names its own, so one App of Apps can span several Hosts. |
| **Child Application** | An Application declared in an App of Apps. Its settings — and Rollback — are owned by Git, not the UI. A manual Sync of the parent also syncs children that are OutOfSync or whose last Sync failed; a Refresh of the parent refreshes all children. |
| **Host** | A Docker engine Applications are deployed to. `local` (the mounted Docker socket) is built in. |
| **Sync** | Making the running containers match the compose files at the target Revision (commit). Each attempt is a Sync Operation, recorded as Running, Succeeded or Failed. |
| **Sync Status** | **Synced**, **OutOfSync** (Live State differs, or a new Revision is waiting), or **Unknown** (the desired state cannot be determined, e.g. Git is unreachable). |
| **Health Status** | **Healthy**, **Progressing**, **Degraded**, **Missing**, or **Unknown** (the Host is unreachable), from container state and healthchecks. A container that exited 0 counts as Healthy (one-shot jobs) — unless its service has a restart policy, then it is long-running and stopped means Degraded. |
| **Rollback** | A Sync to an earlier Revision from the history. It turns Auto-Sync off so the next commit does not undo it; a manual Sync to the target Revision ends it. |

**Sync Policy**, per Application:

- **Auto-Sync** — Sync whenever the repository moves to a new Revision.
- **Self-Heal** — also Sync when the Live State drifts without a new commit (a container was stopped, removed or changed by hand). Requires Auto-Sync.
- **Prune** — what happens to services no longer declared (removed from the compose files, or their profile switched off): with Prune **off**, a Sync *stops* their containers; with Prune **on**, it *deletes* their containers and the project networks no longer declared (a network still used by any container is kept and shown as prune pending). Volumes are kept. Prune is also available as a one-off Sync option.
- **Prune volumes** — a sub-option of Prune (off by default, and turned off with Prune): a Sync also *deletes* the project's named volumes no longer declared, **with their data**, unless a container (of any project, even a stopped one) still uses them. External volumes, volumes OmegaCD's Compose project did not create, volumes of profile-disabled services and host paths are never touched. The Diff tab lists undeclared networks and volumes under *Would be pruned*; they never make an Application OutOfSync.

**Change detection.** Compose recreates a container when its service definition changes. OmegaCD additionally hashes the *content* of file-based `configs:` and `secrets:` and of bind-mounted regular files inside the Application's path into a `dev.omegacd.content-hash` label — so editing `nginx.conf` in Git recreates the container that mounts it, and shows up as OutOfSync until synced. Images are pulled by each service's `pull_policy` (Compose default: only when missing) — push a new tag rather than overwriting one.

## Defining Applications

Create Applications in the UI (**New Application**), with the CLI, or declaratively in Git with an **App of Apps**. Point an Application with kind *App of Apps* at a directory of definitions; every `*.yaml` / `*.yml` file directly in it may hold several documents:

```yaml
# platform/apps.yaml — the path of the App of Apps "platform"
name: web
repoURL: https://github.com/acme/deploy.git
path: apps/web                 # directory with the compose files
targetRevision: main           # branch, tag or commit
composeFiles: [compose.yaml]   # default: the first of compose.yaml, compose.yml, docker-compose.yaml, docker-compose.yml
profiles: [prod]               # optional Compose profiles
host: edge-01                  # required: the Host to deploy to (local for OmegaCD's own Docker engine)
autoSync: true
selfHeal: true
prune: true
pruneVolumes: false            # with prune: also delete volumes no longer declared (data is lost)
---
name: cache
repoURL: https://github.com/acme/deploy.git
path: apps/cache
host: edge-01
autoSync: true
selfHeal: false
prune: false
```

`omegacd app export NAME` (or **Export YAML** in the UI) writes this format for an existing Application, and `omegacd app create -f FILE` reads it. Every definition in an App of Apps names its `host:` (`local` for OmegaCD's own Docker engine): there is no default, and a definition without one fails the App of Apps' Sync, so nothing is deployed to a Host Git didn't name. Nested App of Apps are not supported. `adopt: true` lets an Application take over an existing, unmanaged Compose project of the same name.

![App of Apps](assets/screenshots/app-of-apps.png)

**What each action does on an App of Apps:**

| Action | Order | Children |
| --- | --- | --- |
| **Refresh** | children first, then the App of Apps (its status sums up theirs) | all re-read Git — one Refresh after fixing a Git credential brings them all back |
| **Sync** (manual) | the App of Apps first (creates, updates, with Prune deletes the definitions), then children | only those OutOfSync or whose last Sync failed; Synced ones are not touched |
| **Auto-Sync** | the App of Apps only | each child follows its own Sync Policy |

The App of Apps shows the worst Health of its children.

**Recommended Sync Policies:**

- **The App of Apps:** Auto-Sync on, Prune **off**. A definition removed from Git then only marks its Application *not in Git* and keeps it running until you delete it — a bad commit can't take services down.
- **Stateless Applications** (proxies, dashboards, monitoring): Auto-Sync and Self-Heal on; a push deploys them.
- **Applications with a database** (WordPress, Forgejo, analytics): Auto-Sync off, or on without Prune — upgrades there often want a backup or a migration first.
- **Prune volumes** only where losing the data is fine; never on a database Application.

**Labels.** Every container OmegaCD creates carries `managed-by=omegacd` and `dev.omegacd.application=<name>` (on Docker and Podman alike), so `docker ps --filter label=managed-by=omegacd` lists everything OmegaCD manages. Containers created before an upgrade get the label on their next recreate.

## Where your data lives

OmegaCD makes each Application's work dir match Git on every Sync (files not in Git are deleted), so **data must not live there**. Only named volumes and absolute paths keep data:

| In your compose file | Where the data is | Kept across reboots, Syncs and deletes? |
|---|---|---|
| `- media:/data` (named volume) | Docker's volume storage | ✅ kept, even with Prune — deleted only by **Prune volumes** once no longer declared, or by deleting the Application with **Also delete its volumes** (`app delete --volumes`) |
| `- /opt/media/torrents:/data` (absolute path) | that folder **on the Host running the container** | ✅ OmegaCD never touches it; a missing folder is created by Docker (as root, so `chown` it for non-root images) |
| `- ./nginx.conf:/etc/nginx/nginx.conf:ro` | a file from Git | ✅ restored from Git; changing it recreates the container |
| `- ./conf.d:/etc/nginx/conf.d:ro` | a folder from Git | ✅ for config only: anything a container writes into it is deleted on the next Sync (a `.gitkeep` doesn't make it a data folder) |
| `- ./data:/data` (not in Git) | — | ⛔ **refused**: every Sync deletes files that are not in Git, so the Sync fails and asks for a named volume or an absolute path. The same goes for `../` paths |

Changes *inside* data folders and volumes never count as drift. Back up volumes and data folders yourself (e.g. restic or borg) — OmegaCD doesn't.

## Git access

Public repositories need no setup; the New Application form checks access as you type (public / private with a credential / denied, with an inline *Add credential*). For private ones, add a credential under **Settings → Repository credentials** (for reading Git repositories), matched by URL prefix: a username and password/token for HTTPS, or an SSH private key. Private images need **Settings → Registry credentials** (for pulling Docker images), keyed by registry host such as `ghcr.io` — not a URL.

![New Application form reporting a public repository](assets/screenshots/new-app.png)

SSH host keys are always verified against the `known_hosts` file (`OMEGACD_SSH_KNOWN_HOSTS`, default `/etc/omegacd/known_hosts`):

```sh
ssh-keyscan github.com gitlab.com >> known_hosts   # verify the fingerprints against your provider's docs
```

## Remote Hosts

Applications deploy to the built-in `local` Host by default. Add more Docker engines under **Settings → Hosts** and pick one per Application:

- **`ssh://deploy@server[:port]`** — give the Host an SSH private key for a user allowed to use Docker on the server. The server's host key must be in the same `known_hosts` file (`ssh-keyscan server >> known_hosts`).
- **`tcp://server:2376`** — Docker over TLS; give the Host its CA, client certificate and key (PEM).

Each remote Host gets its own [Host Key](#secrets) when it is added; nothing deploys to a Host without one.

On Sync, the Application's files are copied to the Host's work dir (default `/var/lib/omegacd`, set per Host) so bind mounts, configs and secrets resolve there. It is on disk, so after the Host reboots its containers come back with their files; decrypted secret files are kept there too (mode 0600) — use disk encryption if that matters to you. Values from env files are stored by Docker in the container configuration in any case. To move an Application to another Host, delete and recreate it.

![Settings → Hosts](assets/screenshots/settings-hosts.png)

## Secrets

Commit secrets encrypted with [SOPS](https://github.com/getsops/sops) to the **Host Key** of the Host the Application deploys to. Each Host has its own age key, so a key leaked from one server exposes only that server's secrets:

- **`local`** uses the **Master Key** — the age key in `OMEGACD_AGE_KEY_FILE` (`age-keygen -y age.key` prints its public key). It also encrypts everything OmegaCD stores, including the other Hosts' keys.
- **Remote Hosts** get a Host Key generated when they are added; or import one made on the server with `age-keygen`. A remote Host without a Host Key accepts no Syncs.

**Settings → Hosts** shows each Host's public key, and the **Keys** dialog has the ready `.sops.yaml` rule for it (**Copy .sops.yaml for all Hosts** gives one rule per Host). Private keys are never shown or downloadable, in the UI, API or CLI (`omegacd host key show|generate|import --file|remove`). Back up the Master Key together with the data directory: that is all it takes to restore every Host Key.

```yaml
# .sops.yaml — the paths are up to you
creation_rules:
  - path_regex: apps/prod/.*\.sops\.
    age: age1...   # Host Key of "prod"
  - path_regex: apps/local/.*\.sops\.
    age: age1...   # the Master Key
```

Decryption uses age keys only: files encrypted solely with PGP or a cloud KMS are not decrypted.

**Rotating a Host Key:** *Generate new key* (or import one) makes it current; the old key becomes *previous* and is still tried when decrypting, so Syncs keep working. Re-encrypt the Host's files with `sops updatekeys` after updating `.sops.yaml`, push, then remove the previous key. A file no key of its Host can decrypt fails the Sync with the keys it is encrypted for and the keys the Host has.

Any file whose name contains `.sops.` is decrypted at Sync time, with `.sops.` removed from the name — `app.sops.env` becomes `app.env`, `db-password.sops.txt` becomes `db-password.txt`. The work dir keeps only the decrypted file, which is what the containers read; the encrypted one stays in Git:

```sh
sops encrypt --age age1... --input-type dotenv --output-type dotenv app.env > app.sops.env
sops encrypt --age age1... --input-type binary --output-type binary db-password.txt > db-password.sops.txt
rm app.env db-password.txt   # commit only the .sops. files
```

```yaml
services:
  app:
    image: ghcr.io/acme/app:1.4.2
    env_file: app.env          # decrypted from app.sops.env
    secrets: [db_password]
secrets:
  db_password:
    file: ./db-password.txt    # decrypted from db-password.sops.txt
```

The format is taken from the decrypted file's extension: `.yaml`/`.yml`, `.json`, `.env`/`.dotenv`, `.ini`, anything else binary. Plaintext exists only in the Application's work dir (mode 0600; on the local machine `/run` is a tmpfs on most Linux hosts, on [remote Hosts](#remote-hosts) it is on disk) — never in Git and never in the UI, which shows only the keys of decrypted env files:

![A decrypted env file in the UI: keys only, every value hidden](assets/screenshots/secret-file.png)

### What reaches the disk

How a secret reaches the container decides where it ends up:

| In your compose file | Where the plaintext is |
| --- | --- |
| `env_file: app.env` or `${VAR}` from a decrypted `.env` | the work dir, **and Docker's container configuration on disk** (`/var/lib/docker`, readable with `docker inspect`) — that is how Docker passes environment variables, with or without OmegaCD |
| `secrets: [db_password]` with `file: ./db-password.txt` | **only the work dir**: with the default `/run` it never touches disk; the app reads `/run/secrets/db_password` |

For a secret that must stay off disk, use a Compose secret file and, where the image supports it, a `*_FILE` variable (`MARIADB_PASSWORD_FILE: /run/secrets/db_password`).

## Webhooks

Polling (`OMEGACD_POLL_INTERVAL`, default 3m) always works; webhooks make Syncs start within seconds of a push. Add a push webhook pointing to:

| Provider | URL | Secret |
| --- | --- | --- |
| GitHub | `https://omegacd.example.com/api/webhooks/github` | "Secret" field, checked via `X-Hub-Signature-256` |
| GitLab | `https://omegacd.example.com/api/webhooks/gitlab` | "Secret token", checked via `X-Gitlab-Token` |
| Gitea / Forgejo | `https://omegacd.example.com/api/webhooks/gitea` | "Secret", checked via the HMAC signature header |

Set the same value in `OMEGACD_WEBHOOK_SECRET`. A webhook only triggers a refresh of the Applications using that repository; without a secret anyone who can reach the URL can trigger refreshes, so set one.

**Polling with webhooks.** Webhooks are always on; polling is the safety net for a push OmegaCD missed (it was down, or the Git host could not reach it). With webhooks set up, a long interval like `OMEGACD_POLL_INTERVAL=1h` is a good fit; `0` turns polling off entirely — changes then arrive only through webhooks, **Refresh**, or a restart of OmegaCD, which checks everything once.

## CLI

The same binary is also the client: `omegacd` (or `omegacd server`) runs the server, any other command talks to one. On a laptop or in CI, get it from [Releases](https://github.com/obleey/omegacd/releases) (see [Option 3](#option-3-cli-only)). Create an API token under **Settings → API tokens**, then log in once:

```sh
omegacd login --server https://omegacd.example.com --token ocd_...   # saved to ~/.config/omegacd/config.json (0600)
```

### Everyday use

```sh
omegacd app list                          # all Applications, Child Applications indented under their App of Apps
omegacd app get web                       # status, Revisions, Sync Policy; -o json for the API object
omegacd app diff web                      # what the next Sync changes (exit 1 if anything)
omegacd app sync web --wait               # Sync, then wait until it succeeded and the app is Healthy
omegacd app logs web nginx -f --tail 50   # follow one service's logs
omegacd app history web                   # past Sync Operations with their IDs
omegacd app rollback web 42 --wait        # back to the Revision of operation 42 (turns Auto-Sync off)
omegacd app refresh platform              # re-read Git now; on an App of Apps, its children too
omegacd app export web > web.yaml         # the definition, for an App of Apps folder

# create an Application from the command line (or app create -f web.yaml)
omegacd app create web --repo https://github.com/acme/deploy --path apps/web --host vps --auto-sync --self-heal

# delete it: containers too (asks first); --volumes also deletes its named volumes and their data
omegacd app delete web
```

### In CI

Pass the server and token as environment variables, and let `--wait` fail the job when a deployment fails:

```yaml
# GitHub Actions, after pushing a new image tag to the deploy repo
- run: omegacd app sync myapp --wait --timeout 5m
  env:
    OMEGACD_SERVER: https://omegacd.example.com
    OMEGACD_TOKEN: ${{ secrets.OMEGACD_TOKEN }}
```

`--wait` exits 0 when the Sync Operation succeeded and the Application is Healthy, and 1 when the operation failed, the Application is Degraded, or `--timeout` (default 10m) passes. `--no-health` stops after the operation. `app diff` exits 0 without differences, 1 with, 2 on an error — handy as a check.

### Moving an existing server to OmegaCD

Stacks you started by hand with `docker compose up` can move into Git without downtime planning. `omegacd import` runs **on your laptop, in your repository's checkout**, and reads the stacks running on a server over your own SSH (keys, agent, `~/.ssh/config`) — nothing is installed there and nothing on the server changes:

```sh
cd ~/code/server-apps                      # your (possibly brand-new) Git repo
omegacd import --ssh deploy@vps.example.com --host vps --dry-run   # see what would happen
omegacd import --ssh deploy@vps.example.com --host vps             # write it
```

```text
✓ blog → apps/blog (2 services)
    config   nginx.conf
    data     wp-content → /opt/stacks/blog/wp-content (stays on the host)
    secret   db.env → db.sops.env, encrypted for age1qv7…8x2c
    moved    wordpress: WORDPRESS_DB_PASSWORD from the compose file into the encrypted .sops.env
    takeover recreates its containers once (they mount files that now come from Git)
✓ uptime → apps/uptime (1 service)
    takeover keeps its containers running
```

For each stack it:

- copies the compose files and the **config** they mount (files up to 1 MB, read-only folders up to 1 MB) into `apps/<name>/`;
- leaves **data** where it is: writable or big folders become absolute paths on the server (`./wp-content` → `/opt/stacks/blog/wp-content`), named volumes stay as they are;
- **encrypts secrets** with the server's Host Key: env files with password-like keys become `*.sops.env`, Compose secret files and bind-mounted files that hold a key or credential (by name, or by content: private keys, age keys, `password:`/`token=` settings) `*.sops.*` — never as is; one without a file extension is left out with a warning — and passwords written in `environment:` move into an encrypted `.sops.env` (the compose file then says `${WORDPRESS_DB_PASSWORD}`). The key is looked up on your OmegaCD by `--host`, or given with `--recipient age1…`;
- adds a definition to `platform/apps.yaml` with `host: vps` and `adopt: true`, Auto-Sync **off**;
- says what the takeover will do: containers that only use images and volumes keep running; those mounting files from the stack's folder are recreated once.

Then review with `git diff`, commit and push, add the server under **Settings → Hosts** (if it isn't there yet), and create an App of Apps with path `platform`. Check each Application's Diff and Sync it to take its stack over. Without `--ssh`, `import` reads the local Docker engine; name stacks (`omegacd import blog uptime`) to import only those. Stacks already managed by OmegaCD or already defined are skipped, and an existing `apps/<name>` is only overwritten with `--force`.

### Hosts and keys

```sh
omegacd host list                          # Hosts with status, Docker version and Host Key
omegacd host test vps                      # can OmegaCD reach its Docker engine?
omegacd host key show vps                  # the public key, for .sops.yaml
omegacd host key generate vps              # a new Host Key; the old one stays as "previous" until removed
omegacd host key import vps --file key.txt # use a key made elsewhere (- reads stdin)
omegacd keygen --out age.key               # a new age key, e.g. the Master Key for a fresh install
```

### All commands

| Command | |
| --- | --- |
| `login --server URL --token TOKEN`, `logout`, `version` | |
| `app list`, `app get NAME` | `-o json` for the raw API object |
| `app create NAME --repo URL [--path P] [--revision R] [--file F]... [--profile P]... [--host H] [--kind apps] [--auto-sync] [--self-heal] [--prune [--prune-volumes]] [--adopt]` | or `app create -f app.yaml` |
| `app sync NAME [--prune] [--wait] [--no-health] [--timeout 10m]` | |
| `app refresh NAME` | re-read Git and the Live State now; an App of Apps refreshes its children too |
| `app diff NAME` | exit 0: no differences, 1: differences, 2: error |
| `app history NAME`, `app rollback NAME OP_ID [--wait]` | |
| `app wait NAME [--sync] [--health] [--timeout 10m]` | |
| `app logs NAME [CONTAINER_OR_SERVICE] [-f] [--tail N]` | |
| `app export NAME` | Application definition YAML |
| `app delete NAME [--cascade=false \| --volumes] [--yes]` | asks first on a terminal; `--cascade=false` keeps the containers; `--volumes` also deletes the volumes and their data |
| `import [STACK]... [--ssh user@host --host NAME] [--recipient age1…] [--apps-dir apps] [--definitions platform/apps.yaml] [--repo-url URL] [--dry-run] [--force]` | run in your repository's checkout — see [Moving an existing server](#moving-an-existing-server-to-omegacd) |
| `host list`, `host test NAME` | |
| `host key show HOST`, `host key generate HOST`, `host key import HOST --file PATH`, `host key remove HOST RECIPIENT` | private keys are read from a file or stdin, never from the command line, and never shown |
| `keygen [--out FILE]` | a new age key, like `age-keygen`; works offline, never overwrites a file |

Every command talking to a server takes `--server`, `--token` and `-o table|json`; `OMEGACD_SERVER` and `OMEGACD_TOKEN` override the saved login. Colors are used only on a terminal and honour `NO_COLOR`.

![History and Rollback](assets/screenshots/history.png)

## Configuration

All server settings are environment variables:

| Variable | Default | |
| --- | --- | --- |
| `OMEGACD_ADDR` | `:8080` | Listen address |
| `OMEGACD_DATA` | `/data` | Database (`omegacd.db`), Git cache and keys |
| `OMEGACD_WORKDIR` | `/run/omegacd` | Where Application files are written on Sync; same path on host and container |
| `OMEGACD_AGE_KEY_FILE` | `/etc/omegacd/age.key` | age identity: seals stored credentials and decrypts SOPS files. Required |
| `OMEGACD_SSH_KNOWN_HOSTS` | `/etc/omegacd/known_hosts` | Host keys for Git over SSH and `ssh://` Hosts |
| `OMEGACD_LOCAL_HOST` | `true` | `false` where no Docker engine runs next to OmegaCD (Kubernetes): the built-in `local` Host is hidden and refused, Applications go to remote Hosts |
| `OMEGACD_POLL_INTERVAL` | `3m` | How often Git is polled (Go duration); `0` turns polling off (webhooks only, see [Webhooks](#webhooks)) |
| `OMEGACD_ADMIN_PASSWORD` | *(generated)* | Initial `admin` password, used only on first start. If unset, a random one is logged and must be changed on first login |
| `OMEGACD_WEBHOOK_SECRET` | *(none)* | Shared secret for webhooks |
| `OMEGACD_TRUSTED_PROXIES` | *(none)* | Comma-separated IPs/CIDRs of reverse proxies whose `X-Forwarded-For` is trusted |

CLI: `OMEGACD_SERVER`, `OMEGACD_TOKEN`, `NO_COLOR`.

## Metrics

`GET /metrics` (no authentication) serves Prometheus metrics:

- `omegacd_sync_operations_total{app, phase}` — finished Sync Operations
- `omegacd_sync_duration_seconds{app}` — Sync Operation duration histogram
- `omegacd_app_info{app, sync_status, health}` — always 1; one series per Application with its current status
- Go runtime and process metrics

`GET /healthz` returns `ok` for liveness checks.

## Security

- **Secrets stay secret.** Passwords, tokens and private keys are write-only in the API and UI; decrypted SOPS files are never shown (env files show keys only).
- **Credentials are sealed at rest** with the age key (repository, registry and Host credentials in the database).
- **API tokens** (`ocd_…`) are shown once and stored only as SHA-256 hashes; the admin password is stored as a bcrypt hash.
- **Sessions** are HttpOnly, SameSite=Lax cookies (Secure behind HTTPS) that expire after 24 h of inactivity. State-changing requests from the browser must carry an `X-Requested-With` header, which cross-site forms cannot send (CSRF protection).
- **Login throttling** with exponential backoff per client IP. Behind a reverse proxy, set `OMEGACD_TRUSTED_PROXIES` so the real client IP is used.
- **Host keys** are always verified for SSH (Git and Hosts); there is no "accept unknown host" mode.
- OmegaCD has full control of the Docker engines it manages — treat access to it like root access to those hosts.

Please report vulnerabilities privately, see [SECURITY.md](SECURITY.md).

## Development

Requires Go (version in `go.mod`), Node.js 24 and a running Docker engine.

```sh
make build              # web UI, then bin/omegacd (VERSION from git describe)
make test               # unit tests
make test-integration   # integration tests against the local Docker engine (incl. docker:dind for remote Hosts)
make stop               # stop a locally running omegacd (only the process listening on :8080)
make run                # build and run with data in ./data (OMEGACD_WORKDIR=/some/dir to move the work dir)
make docker             # build the container image
make up / make down     # run that image with deploy/compose.yaml, same data and work dir as make run
```

| Path | |
| --- | --- |
| `cmd/omegacd` | entry point: server and CLI |
| `internal/api` | HTTP API, auth, webhooks, metrics |
| `internal/controller` | reconcile loop, Sync Operations, Sync Policy, App of Apps |
| `internal/engine` | Docker Compose (as a library), Hosts, diff, health |
| `internal/gitsrc` | Git fetch, diffs, webhook parsing |
| `internal/secrets`, `internal/crypto` | SOPS decryption, sealing with age |
| `internal/store` | SQLite storage |
| `internal/cli` | the CLI client |
| `web` | SvelteKit UI, embedded into the binary |

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). What is tested, automatically and by hand, is in [TESTING.md](TESTING.md).

## Roadmap

See [ROADMAP.md](ROADMAP.md) — next up: sync hooks and notifications. Related, planned as a separate project: a SOPS web GUI to encrypt secrets in the browser.

## License

[Apache-2.0](LICENSE)
