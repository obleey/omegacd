# Secrets are decrypted only on the Agent

An Agent generates its own age Host Key and decrypts Encrypted Secrets in memory, writing plaintext only into the Application's folder on its Host; the Control Plane ships ciphertext and holds public keys only. This supersedes ADR 0002, whose central decryption was a stopgap until Compose ran on the Host. Secret sources plug in behind one interface on the Agent (SOPS with age now; 1Password, Vault, OpenBao and others later), with their access credentials in the Agent's configuration, never in the Control Plane or Git.

## Consequences

- A compromised Control Plane can't read any secret; a compromised Host exposes only its own.
- The UI can't show decrypted values: it shows key names reported by the Agent.
- Losing a Host's state dir loses its Host Key: re-encrypt that Host's secrets for the new key (documented).
