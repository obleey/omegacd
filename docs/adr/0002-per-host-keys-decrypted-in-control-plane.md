# Per-Host Keys, decrypted in the control plane (for now)

**Superseded in v0.2 by [ADR 0005](0005-decrypt-on-the-agent.md)**: Agents decrypt on the Host; this remains the model of the v0.1 line.

Each Host has its own Host Key (age), like each Kubernetes cluster has its own Sealed Secrets key, so a key leaked from one server only exposes that server's secrets and teams only need the public keys of the Hosts they deploy to. OmegaCD itself stores the Host Keys (encrypted with the Master Key) and decrypts at Sync time, because Docker Compose reads `env_file` values on the side that runs Compose — OmegaCD, not the Host. Keeping the private key only on the Host requires running Compose there (a future agent mode); until then the control plane holds all Host Keys.

## Consequences

- Compromise of OmegaCD (or its data directory plus Master Key) exposes every Host's secrets; a compromised Host exposes only its own.
- Private Host Keys are never shown or downloadable; backups are the Master Key plus the data directory. Keys meant to also live on the server are generated there and imported.
- Rotation keeps previous keys for decryption until removed, so Syncs don't break while files are re-encrypted.

## Considered Options

- **Decrypt on the Host (key never leaves it)**: the Sealed Secrets model; deferred to agent mode because of `env_file` client-side resolution.
- **One global key for everything**: today's model; one leak exposes all Hosts.
