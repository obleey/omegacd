# omegacd

GitOps continuous delivery for Docker: keeps Compose projects running on Docker hosts in line with what is declared in Git.

## Language

### Deployment

**Application**:
A Compose project whose desired state lives at a path in a Repository and is deployed to one Host.
_Avoid_: App, stack, service, project

**App of Apps**:
An Application whose path holds Application definitions instead of compose files; syncing it creates, updates and (with Prune) deletes its Child Applications.
_Avoid_: ApplicationSet, parent app, meta app

**Child Application**:
An Application declared in an App of Apps' path; its settings are owned by Git, not the UI.
_Avoid_: Sub-app, managed app

**Host**:
A Docker engine that omegacd deploys Applications to.
_Avoid_: Server, node, cluster, target, edge host, workload node

**Agent**:
The omegacd process running on a Host, connected to the Control Plane, that pulls its Applications' Desired State and runs Compose there.
_Avoid_: Edge agent, worker, daemon, node

**Enrollment**:
Connecting an Agent to a Control Plane: a Host named by an admin gets a single-use invite, the Agent registers its own key pairs with it, and an admin approves the key's fingerprint.
_Avoid_: Registration, onboarding, pairing, join token

**Control Plane**:
The central omegacd server that holds Applications, their sync history and settings, serves the UI and API, and tells Agents what to deploy.
_Avoid_: Ops hub, central engine, token router, master

**Source Server**:
The omegacd component that fetches a Repository at a Revision, validates it and renders its Desired State with secrets still encrypted, for the Control Plane to hand to Agents; the only part holding Git credentials.
_Avoid_: Repo server, repo-svc, manifest server, revision server

**Repository**:
A Git repository holding the compose files of one or more Applications.
_Avoid_: Repo source, origin

**Revision**:
The Git commit an Application is, or should be, deployed from.
_Avoid_: Version, ref, SHA

### State

**Desired State**:
What the compose files at an Application's Revision say should be running.
_Avoid_: Target state, spec, manifest

**Live State**:
What is actually running on the Host for an Application.
_Avoid_: Actual state, current state, runtime

**Sync**:
Making the Live State of an Application match its Desired State.
_Avoid_: Deploy, apply, rollout, reconcile

**Sync Operation**:
One attempt to Sync an Application to a Revision, recorded in its sync history as Running, Succeeded or Failed.
_Avoid_: Deployment, job, run

**Managed**:
A Compose project on a Host that omegacd owns as an Application.
_Avoid_: Tracked, owned

**Adopt**:
Making an existing, unmanaged Compose project on a Host Managed by an Application.
_Avoid_: Import, take over

**Sync Status**:
Whether an Application's Live State matches its Desired State: Synced, OutOfSync, or Unknown when the Desired State cannot be determined.
_Avoid_: Drift status, state

**Health Status**:
Whether an Application's running containers are working, independent of Sync Status: Healthy, Progressing, Degraded, Missing, or Unknown when its Host cannot be reached. A container that exited 0 counts as Healthy, unless its service has a restart policy (then it is long-running, and stopped means Degraded and drifted).
_Avoid_: Status, liveness

**Rollback**:
A Sync to an earlier Revision from the Application's sync history; it turns Auto-Sync off.
_Avoid_: Revert, undo

### Sync Policy

**Auto-Sync**:
An Application setting that Syncs automatically whenever the Repository moves to a new Revision.
_Avoid_: Auto-deploy, continuous sync

**Self-Heal**:
An Application setting that Syncs automatically when the Live State drifts without a new Revision.
_Avoid_: Auto-repair, drift correction

**Prune**:
An Application setting (or a one-off Sync option) that deletes containers and networks no longer in the Desired State; a network still used by any container is kept (prune pending). Without it, a Sync only stops containers of services no longer declared. Volumes are pruned only with Prune volumes.
_Avoid_: Cleanup, garbage collection, remove orphans

**Prune volumes**:
A sub-option of Prune (it has no effect, and is saved as off, without Prune) that also deletes the Application's Compose project volumes no longer declared, with their data, once no container uses them. Never external volumes, volumes of profile-disabled services, or host paths. Deleting an Application can separately delete its volumes ("Also delete its volumes", `app delete --volumes`).
_Avoid_: Volume cleanup, remove volumes

### Secrets

**Encrypted Secret**:
A SOPS-encrypted file in a Repository that is decrypted only at Sync time.
_Avoid_: Secret file, vault, credential

**Host Key**:
The age key pair of one Host, used to decrypt the Encrypted Secrets of Applications deployed there; its public key is what you encrypt to.
_Avoid_: Sealing key, cluster key, server key

**Master Key**:
The age key OmegaCD starts with; it encrypts OmegaCD's stored credentials and Host Keys, and is the Host Key of the local Host.
_Avoid_: Root key, admin key, OmegaCD key

**Secret Source**:
Where an Encrypted Secret's plaintext comes from; SOPS with age keys is the only one for now.
_Avoid_: secretsFrom, provider, backend, ConfigMap
