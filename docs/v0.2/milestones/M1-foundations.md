# M1 · Foundations

The pieces everything else builds on: the release branch, the message schema, and the Source Server behind an
interface. See [architecture](../architecture.md#source-server-q5-q12-q13-q15-q17-q31-q45-q46).

### M1-01 · release-0.1 branch
**Status:** done (2026-11-06, from rc.10 `25de111`)
**Depends on:** —

v0.1 fixes go to `release-0.1`; `main` is v0.2 from now on (Q54). Tag `v0.1.0` from that branch at release time (M8-06).

**Done when:** the branch exists on GitHub and README/CONTRIBUTING say where 0.1 fixes go.

### M1-02 · Protobuf message schema
**Status:** done (2026-11-06, PR #21)
**Depends on:** —

Define every Agent ↔ Control Plane message in `proto/omegacd/agent/v1/*.proto` (Q20, Q27): an envelope with
`oneof`: hello (Agent version, protocol version, engine, rootless, OS/arch, hostname), challenge, signed response,
desired state (app, Revision, file list `(path, mode, sha256)`, compose files, profiles), blob request and blob
chunk, resync (per-app checksums), status/drift report, Sync Operation report, logs request and log chunk,
offline outbox entries, error. Generated Go in `internal/proto/…`, generation via `make proto` (pinned tool
versions, generated code committed). The Source Server API (Resolve, Render, Blob, CheckAccess, credential CRUD)
gets its own `source/v1` service definition for gRPC (M1-05).

**Done when:** schemas compile, generated code is committed and reproducible, a round-trip test encodes and
decodes every message, and the schema has a short README (versioning rules: add fields, never reuse numbers).

### M1-03 · Source Server package with its interface, in-process
**Status:** done (2026-11-06, PR #71)
**Depends on:** M1-02, M1-04

New `internal/source` package (Q5, Q12, Q46): Resolve(repo, ref) → commit; Render(app spec) → file list with
hashes + metadata + warnings/errors; Blob(hash); CheckAccess(repo). Move `internal/gitsrc` behind it, and move the
validations that today live in the engine/controller (refuse `build:`, refuse relative mounts not in Git,
plain-text secret warnings, App of Apps definition parsing) into Render. Rendering never needs a secret: merge
compose files, apply profiles; anything that needs a decrypted `.env` is left to the Agent.

**Done when:** the Control Plane (still in v0.1 mode) gets Desired State only through this interface, all existing
gitsrc/validation tests pass in their new home, and Render is deterministic (same commit → same hashes).

### M1-04 · Content-addressed blob store
**Status:** done (2026-11-06, PR #70)
**Depends on:** —

A small shared package (Q27, Q31): put/get/has by sha256 on disk, atomic writes, references from Revisions, garbage
collection by reference with a grace period. Used by the Control Plane (every delivered payload, kept) and the Agent
(its cache, last N Revisions).

**Done when:** unit tests cover concurrent puts, corruption detection (hash mismatch is an error), GC never removes
a referenced blob, and crash-safety (a killed write leaves no half file under its hash).

### M1-05 · Standalone Source Server over gRPC with mTLS
**Status:** in progress (feat/m1-05-grpc-source-server)
**Depends on:** M1-03

`omegacd source-server` serves the `source/v1` API over gRPC with **mutual TLS** (Q15, Q45, Q51); the Control Plane
uses it when `OMEGACD_SOURCE_SERVER=https://…` is set, else in-process. A `keygen`-like command generates the CA,
server and client certificates at install. It refuses anything without the Control Plane's client certificate.

**Done when:** an integration test runs Control Plane + standalone Source Server, a Sync renders through it, a
client without the certificate is refused, and the docs explain TLS passthrough for proxies in between.

### M1-06 · Git credentials owned by the Source Server
**Status:** todo
**Depends on:** M1-03, M1-05

The Source Server stores Repository credentials sealed with **its own key** (generated at its first start, in its
data dir); credential CRUD and the access check are its API (Q13, Q40). The Control Plane passes new credentials
through and keeps only names and URL prefixes to display.

**Done when:** the Control Plane's database holds no Git secret (test), credentials survive a Source Server restart,
the UI access check works through it, and losing the Control Plane's database loses no credential.
